package database

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"shift.dev/shift/internal/model"
)

// scim.go: SCIM v2 user provisioning. The idP writes users through /scim/v2
// endpoints; each SCIM user maps to one row in users keyed by external_id
// (the idP's user id) plus membership in the provisioning organization.
// Deactivation sets disabled_at rather than deleting the row, so history and
// audit trails keep pointing at a real account.

// SCIMUser is the provisioned view of an account: what the idP knows and
// controls.
type SCIMUser struct {
	ID          string
	ExternalID  string
	Email       string
	DisplayName string
	Active      bool
}

// ErrSCIMConflict marks an email or external id that already belongs to a
// different user, which the protocol reports as 409.
var ErrSCIMConflict = errors.New("an account with this email or external id already exists")

// SCIMCreateUser provisions a user inside one organization.
func (store *Store) SCIMCreateUser(ctx context.Context, organizationID string, user SCIMUser, audit AuditInput) (SCIMUser, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return SCIMUser{}, err
	}
	defer tx.Rollback(ctx)
	userID, err := model.NewID()
	if err != nil {
		return SCIMUser{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO users(id,email,password_hash,display_name,external_id,created_at,updated_at) VALUES($1,$2,'',$3,$4,$5,$5)`,
		userID, strings.ToLower(strings.TrimSpace(user.Email)), user.DisplayName, strings.TrimSpace(user.ExternalID), time.Now().UTC()); err != nil {
		if IsConflict(err) {
			return SCIMUser{}, ErrSCIMConflict
		}
		return SCIMUser{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role,created_at) VALUES($1,$2,'viewer',$3)`, organizationID, userID, time.Now().UTC()); err != nil {
		return SCIMUser{}, err
	}
	audit.OrganizationID = organizationID
	audit.ResourceType = "user"
	audit.ResourceID = userID
	if err := insertAudit(ctx, tx, audit); err != nil {
		return SCIMUser{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SCIMUser{}, err
	}
	user.ID = userID
	user.Email = strings.ToLower(strings.TrimSpace(user.Email))
	user.Active = true
	return user, nil
}

// scimUserSelect is the shared read shape: one row joined with its membership
// state so "active" means both the account and its membership exist.
const scimUserSelect = `SELECT u.id,u.external_id,u.email,u.display_name,(u.disabled_at IS NULL) AS active
	FROM users u JOIN organization_members m ON m.user_id=u.id
	WHERE m.organization_id=$1`

// SCIMUserByID reads one provisioned user.
func (store *Store) SCIMUserByID(ctx context.Context, organizationID, userID string) (SCIMUser, error) {
	var user SCIMUser
	err := store.pool.QueryRow(ctx, scimUserSelect+` AND u.id=$2`, organizationID, userID).
		Scan(&user.ID, &user.ExternalID, &user.Email, &user.DisplayName, &user.Active)
	return user, err
}

// SCIMUsers lists the organization's provisioned users, filtered by the
// parsed SCIM filter (clauses ANDed together). Values only ever bind as query
// parameters — attribute and operator names come from fixed whitelists, never
// from the request text.
func (store *Store) SCIMUsers(ctx context.Context, organizationID string, filter []SCIMClause) ([]SCIMUser, error) {
	query := scimUserSelect
	args := []any{organizationID}
	for _, clause := range filter {
		fragment, value, ok := renderSCIMClause(clause, len(args)+1)
		if !ok {
			return nil, fmt.Errorf("unsupported SCIM filter on %q with operator %q", clause.Attribute, clause.Operator)
		}
		query += " AND " + fragment
		if value != nil {
			args = append(args, value)
		}
	}
	rows, err := store.pool.Query(ctx, query+` ORDER BY u.email`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []SCIMUser
	for rows.Next() {
		var user SCIMUser
		if err := rows.Scan(&user.ID, &user.ExternalID, &user.Email, &user.DisplayName, &user.Active); err != nil {
			return nil, err
		}
		result = append(result, user)
	}
	return result, rows.Err()
}

// SCIMClause is one parsed filter comparison. Attribute and Operator hold
// canonical lowercase names; the renderer whitelists both.
type SCIMClause struct {
	Attribute string
	Operator  string
	Value     string
}

// scimAttributes maps the protocol's attribute paths to their canonical
// column names.
var scimAttributes = map[string]string{
	"username":     "email",
	"emails.value": "email",
	"email":        "email",
	"displayname":  "display_name",
	"externalid":   "external_id",
	"active":       "active",
}

// ParseSCIMFilter parses `attribute op value [and attribute op value ...]`
// with the operators eq, co, sw, ew, and pr. Anything else — or, not,
// parentheses, comparisons on attributes the store does not expose — is
// rejected rather than mis-executed.
func ParseSCIMFilter(filter string) ([]SCIMClause, error) {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return nil, nil
	}
	tokens, err := scimTokens(filter)
	if err != nil {
		return nil, err
	}
	var clauses []SCIMClause
	position := 0
	for {
		if position >= len(tokens) {
			return nil, errors.New("SCIM filter ended before a complete comparison")
		}
		attribute, ok := scimAttributes[strings.ToLower(tokens[position])]
		if !ok {
			return nil, fmt.Errorf("SCIM filter attribute %q is not filterable", tokens[position])
		}
		position++
		if position >= len(tokens) {
			return nil, errors.New("SCIM filter comparison is missing its operator")
		}
		operator := strings.ToLower(tokens[position])
		position++
		switch operator {
		case "eq", "co", "sw", "ew":
			if position >= len(tokens) {
				return nil, fmt.Errorf("SCIM filter operator %q is missing its value", operator)
			}
			value, err := scimValue(tokens[position])
			if err != nil {
				return nil, err
			}
			if attribute == "active" {
				if value != "true" && value != "false" {
					return nil, errors.New("the active attribute compares against true or false")
				}
			}
			clauses = append(clauses, SCIMClause{Attribute: attribute, Operator: operator, Value: value})
			position++
		case "pr":
			clauses = append(clauses, SCIMClause{Attribute: attribute, Operator: "pr"})
		default:
			return nil, fmt.Errorf("unsupported SCIM filter operator %q", operator)
		}
		if position >= len(tokens) {
			return clauses, nil
		}
		if !strings.EqualFold(tokens[position], "and") {
			return nil, fmt.Errorf("unsupported SCIM filter connective %q (only \"and\" is supported)", tokens[position])
		}
		position++
		if position >= len(tokens) {
			return nil, errors.New("SCIM filter ended after \"and\"")
		}
	}
}

// scimTokens splits a filter on whitespace, keeping double-quoted strings
// whole. A quote appearing inside a value is rejected — it is not a shape
// this parser is willing to guess about.
func scimTokens(filter string) ([]string, error) {
	var tokens []string
	var current strings.Builder
	inQuotes := false
	flush := func() {
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}
	for _, character := range filter {
		switch {
		case character == '"':
			if inQuotes {
				tokens = append(tokens, current.String())
				current.Reset()
				inQuotes = false
			} else {
				flush()
				inQuotes = true
			}
		case character == ' ' || character == '\t':
			if inQuotes {
				current.WriteRune(character)
			} else {
				flush()
			}
		default:
			current.WriteRune(character)
		}
	}
	if inQuotes {
		return nil, errors.New("SCIM filter has an unterminated quoted value")
	}
	flush()
	return tokens, nil
}

// scimValue unquotes one comparison value.
func scimValue(token string) (string, error) {
	if strings.ContainsAny(token, "\"()") {
		return "", fmt.Errorf("SCIM filter value %q contains unsupported characters", token)
	}
	if token == "" {
		return "", errors.New("SCIM filter value is empty")
	}
	return token, nil
}

// renderSCIMClause turns one clause into a parameterized SQL fragment. The
// second return value is the parameter to bind (nil when the clause needs
// none, as with "pr"); the third reports whether the clause is filterable at
// all. Attribute and operator names come from whitelists — request text never
// reaches the SQL string.
func renderSCIMClause(clause SCIMClause, placeholder int) (string, any, bool) {
	column := map[string]string{
		"email":        "lower(u.email)",
		"display_name": "lower(u.display_name)",
		"external_id":  "lower(u.external_id)",
	}[clause.Attribute]
	if clause.Attribute == "active" {
		if clause.Operator != "eq" {
			return "", nil, false
		}
		return fmt.Sprintf("(u.disabled_at IS NULL) = $%d::boolean", placeholder), clause.Value == "true", true
	}
	if column == "" {
		return "", nil, false
	}
	switch clause.Operator {
	case "eq":
		return fmt.Sprintf("%s = lower($%d)", column, placeholder), clause.Value, true
	case "co":
		return fmt.Sprintf("%s LIKE '%%' || lower($%d) || '%%'", column, placeholder), clause.Value, true
	case "sw":
		return fmt.Sprintf("%s LIKE lower($%d) || '%%'", column, placeholder), clause.Value, true
	case "ew":
		return fmt.Sprintf("%s LIKE '%%' || lower($%d)", column, placeholder), clause.Value, true
	case "pr":
		return fmt.Sprintf("%s <> ''", column), nil, true
	default:
		return "", nil, false
	}
}

// SCIMReplaceUser applies a full replacement of the idP-controlled fields.
func (store *Store) SCIMReplaceUser(ctx context.Context, organizationID, userID string, user SCIMUser, audit AuditInput) (SCIMUser, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return SCIMUser{}, err
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, `UPDATE users SET email=$3,display_name=$4,external_id=$5,disabled_at=CASE WHEN $6::boolean THEN NULL ELSE now() END,updated_at=now()
		WHERE id=$2 AND EXISTS(SELECT 1 FROM organization_members m WHERE m.organization_id=$1 AND m.user_id=users.id)`,
		organizationID, userID, strings.ToLower(strings.TrimSpace(user.Email)), user.DisplayName, strings.TrimSpace(user.ExternalID), user.Active)
	if err != nil {
		return SCIMUser{}, err
	}
	if command.RowsAffected() == 0 {
		return SCIMUser{}, pgx.ErrNoRows
	}
	audit.OrganizationID = organizationID
	audit.ResourceType = "user"
	audit.ResourceID = userID
	if err := insertAudit(ctx, tx, audit); err != nil {
		return SCIMUser{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SCIMUser{}, err
	}
	return store.SCIMUserByID(ctx, organizationID, userID)
}

// SCIMSetActive activates or deactivates a user. Deactivation is what a SCIM
// delete means: the account's sessions stop authenticating immediately, but
// the row survives for the audit trail.
func (store *Store) SCIMSetActive(ctx context.Context, organizationID, userID string, active bool, audit AuditInput) (SCIMUser, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return SCIMUser{}, err
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, `UPDATE users SET disabled_at=CASE WHEN $3::boolean THEN NULL ELSE now() END,updated_at=now()
		WHERE id=$2 AND EXISTS(SELECT 1 FROM organization_members m WHERE m.organization_id=$1 AND m.user_id=users.id)`,
		organizationID, userID, active)
	if err != nil {
		return SCIMUser{}, err
	}
	if command.RowsAffected() == 0 {
		return SCIMUser{}, pgx.ErrNoRows
	}
	if !active {
		// Deactivated accounts lose their live sessions in the same
		// transaction the deactivation commits in.
		if _, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`, userID); err != nil {
			return SCIMUser{}, err
		}
	}
	audit.OrganizationID = organizationID
	audit.ResourceType = "user"
	audit.ResourceID = userID
	if err := insertAudit(ctx, tx, audit); err != nil {
		return SCIMUser{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SCIMUser{}, err
	}
	return store.SCIMUserByID(ctx, organizationID, userID)
}
