package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"shift.dev/shift/internal/database"
)

// scim_routes.go: the SCIM v2 provisioning surface. An identity provider
// writes users through /v1/scim/v2/Users with an API key carrying the "scim"
// scope; each SCIM user maps to one row in users keyed by external_id. SCIM
// delete means deactivation — the row survives so audit trails keep pointing
// at a real account — and deactivation revokes the account's live sessions in
// the same transaction. Only the Users resource is implemented; Groups is not,
// and the discovery endpoints say so.

const (
	scimUserSchema         = "urn:ietf:params:scim:schemas:core:2.0:User"
	scimListResponseSchema = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	scimErrorSchema        = "urn:ietf:params:scim:api:messages:2.0:Error"
	scimPatchSchema        = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	scimProviderSchema     = "urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"
	scimResourceTypeSchema = "urn:ietf:params:scim:schemas:core:2.0:ResourceType"
	scimSchemaSchema       = "urn:ietf:params:scim:schemas:core:2.0:Schema"
	scimContentType        = "application/scim+json"
	scimMaxPage            = 200
)

// scimEmail is one entry of the request's emails array.
type scimEmail struct {
	Value   string `json:"value"`
	Primary bool   `json:"primary"`
}

// scimUserRequest is the body for create and replace. RFC 7644 §3.1 says a
// server ignores attributes it does not recognize, so unlike the control
// plane's own JSON the decoder must not reject unknown fields.
type scimUserRequest struct {
	Schemas     []string    `json:"schemas"`
	ExternalID  string      `json:"externalId"`
	UserName    string      `json:"userName"`
	DisplayName string      `json:"displayName"`
	Active      *bool       `json:"active"`
	Emails      []scimEmail `json:"emails"`
}

// scimPatchOperation is one entry of a PatchOp request's Operations array.
// Value stays raw because "active" carries a boolean and the text attributes
// carry strings.
type scimPatchOperation struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

// scimPatchRequest is the body of a PATCH.
type scimPatchRequest struct {
	Schemas    []string             `json:"schemas"`
	Operations []scimPatchOperation `json:"Operations"`
}

// decodeSCIM mirrors decodeJSON — bounded body, no trailing data — but keeps
// unknown fields, which the protocol requires the server to ignore.
func decodeSCIM(writer http.ResponseWriter, request *http.Request, target any) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, controlBodyLimit)
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(target); err != nil {
		writeSCIMError(writer, http.StatusBadRequest, "the request body is not valid SCIM JSON: "+err.Error())
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeSCIMError(writer, http.StatusBadRequest, "the request contains trailing data")
		return false
	}
	return true
}

// writeSCIM writes a successful SCIM response with the protocol's content
// type.
func writeSCIM(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", scimContentType)
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

// writeSCIMError writes the protocol's error shape: a schema urn, a string
// status, and a detail message.
func writeSCIMError(writer http.ResponseWriter, status int, detail string) {
	writeSCIM(writer, status, map[string]any{
		"schemas": []string{scimErrorSchema},
		"status":  strconv.Itoa(status),
		"detail":  detail,
	})
}

// scimUserResponse renders the stored account in the SCIM User shape. userName
// is the account's email — this control plane keys users by address.
func scimUserResponse(user database.SCIMUser) map[string]any {
	return map[string]any{
		"schemas":     []string{scimUserSchema},
		"id":          user.ID,
		"externalId":  user.ExternalID,
		"userName":    user.Email,
		"displayName": user.DisplayName,
		"active":      user.Active,
		"emails":      []map[string]any{{"value": user.Email, "primary": true}},
		"meta":        map[string]any{"resourceType": "User", "location": "/v1/scim/v2/Users/" + user.ID},
	}
}

// scimRequestUser validates a create/replace body and turns it into the
// store's shape. defaultActive is what an absent "active" means: true on
// create, and on replace the account's current state — a routine attribute
// sync that omits the flag must never re-enable a deactivated account.
func scimRequestUser(input scimUserRequest, defaultActive bool) (database.SCIMUser, error) {
	email, err := normalizeEmail(input.UserName)
	if err != nil {
		return database.SCIMUser{}, errors.New("userName must be the user's email address")
	}
	if len(input.DisplayName) > 200 {
		return database.SCIMUser{}, errors.New("displayName must be at most 200 characters")
	}
	displayName := strings.TrimSpace(input.DisplayName)
	if displayName == "" {
		displayName = email
	}
	externalID := strings.TrimSpace(input.ExternalID)
	if len(externalID) > 256 {
		return database.SCIMUser{}, errors.New("externalId must be at most 256 characters")
	}
	// The emails array, when present, must agree with userName: two addresses
	// for one account is a split brain the store has no place to put.
	for _, entry := range input.Emails {
		address, err := normalizeEmail(entry.Value)
		if err != nil {
			return database.SCIMUser{}, errors.New("each emails entry must be a valid address")
		}
		if !strings.EqualFold(address, email) {
			return database.SCIMUser{}, errors.New("emails must match userName; an account has one address")
		}
	}
	active := defaultActive
	if input.Active != nil {
		active = *input.Active
	}
	return database.SCIMUser{ExternalID: externalID, Email: email, DisplayName: displayName, Active: active}, nil
}

// handleSCIMUsersList answers GET /Users with a ListResponse. Filtering and
// paging both run in SQL: attribute and operator names come from fixed
// whitelists and values only ever bind as parameters, and a large directory
// is never materialized in memory just to be sliced.
func (server *Server) handleSCIMUsersList(writer http.ResponseWriter, request *http.Request) {
	filter, err := database.ParseSCIMFilter(request.URL.Query().Get("filter"))
	if err != nil {
		writeSCIMError(writer, http.StatusBadRequest, err.Error())
		return
	}
	startIndex := int(parseMetadataInt(request.URL.Query().Get("startIndex"), 1))
	if startIndex < 1 {
		startIndex = 1
	}
	count := int(parseMetadataInt(request.URL.Query().Get("count"), 100))
	if count > scimMaxPage {
		count = scimMaxPage
	}
	if count < 0 {
		count = 0
	}
	users, total, err := server.database.SCIMUsers(request.Context(), principalFrom(request.Context()).OrganizationID, filter, database.NormalizePage(count, startIndex-1))
	if err != nil {
		writeSCIMError(writer, http.StatusInternalServerError, "the user list could not be read")
		return
	}
	resources := make([]map[string]any, 0, len(users))
	for _, user := range users {
		resources = append(resources, scimUserResponse(user))
	}
	writeSCIM(writer, http.StatusOK, map[string]any{
		"schemas":      []string{scimListResponseSchema},
		"totalResults": total,
		"startIndex":   startIndex,
		"itemsPerPage": len(resources),
		"Resources":    resources,
	})
}

// handleSCIMUserCreate answers POST /Users. The active attribute is
// readWrite (RFC 7643 §4.1.1): a directory that stages a user as inactive
// gets a disabled account, never a live one.
func (server *Server) handleSCIMUserCreate(writer http.ResponseWriter, request *http.Request) {
	var input scimUserRequest
	if !decodeSCIM(writer, request, &input) {
		return
	}
	user, err := scimRequestUser(input, true)
	if err != nil {
		writeSCIMError(writer, http.StatusBadRequest, err.Error())
		return
	}
	organizationID := principalFrom(request.Context()).OrganizationID
	created, err := server.database.SCIMCreateUser(request.Context(), organizationID, user,
		server.auditInput(request, "scim.user.create", "user", "", map[string]any{"email": user.Email, "external_id": user.ExternalID}))
	if err != nil {
		if errors.Is(err, database.ErrSCIMConflict) || database.IsConflict(err) {
			writeSCIMError(writer, http.StatusConflict, "an account with this email or external id already exists")
			return
		}
		if errors.Is(err, database.ErrSeatCeilingExceeded) {
			writeSCIMError(writer, http.StatusPaymentRequired, "seat count must cover every organization member; purchase more seats before provisioning users")
			return
		}
		writeSCIMError(writer, http.StatusInternalServerError, "the user could not be created")
		return
	}
	if !user.Active {
		deactivated, setActiveErr := server.database.SCIMSetActive(request.Context(), organizationID, created.ID, false,
			server.auditInput(request, "scim.user.create_inactive", "user", created.ID, nil))
		if setActiveErr != nil {
			writeSCIMError(writer, http.StatusInternalServerError, "the user could not be created")
			return
		}
		created = deactivated
	}
	writer.Header().Set("Location", "/v1/scim/v2/Users/"+created.ID)
	writeSCIM(writer, http.StatusCreated, scimUserResponse(created))
}

// handleSCIMUserGet answers GET /Users/{id}.
func (server *Server) handleSCIMUserGet(writer http.ResponseWriter, request *http.Request) {
	user, err := server.database.SCIMUserByID(request.Context(), principalFrom(request.Context()).OrganizationID, request.PathValue("userID"))
	if err != nil {
		if database.IsNotFound(err) {
			writeSCIMError(writer, http.StatusNotFound, "the user was not found")
			return
		}
		writeSCIMError(writer, http.StatusInternalServerError, "the user could not be read")
		return
	}
	writeSCIM(writer, http.StatusOK, scimUserResponse(user))
}

// handleSCIMUserReplace answers PUT /Users/{id}: a full replacement of the
// idP-controlled fields. An omitted "active" preserves the account's current
// state, and any transition to or from the disabled state revokes the
// account's live sessions — deactivation must kill them, and re-enabling must
// never resurrect tokens issued before the deactivation.
func (server *Server) handleSCIMUserReplace(writer http.ResponseWriter, request *http.Request) {
	var input scimUserRequest
	if !decodeSCIM(writer, request, &input) {
		return
	}
	organizationID := principalFrom(request.Context()).OrganizationID
	userID := request.PathValue("userID")
	current, err := server.database.SCIMUserByID(request.Context(), organizationID, userID)
	if err != nil {
		if database.IsNotFound(err) {
			writeSCIMError(writer, http.StatusNotFound, "the user was not found")
			return
		}
		writeSCIMError(writer, http.StatusInternalServerError, "the user could not be read")
		return
	}
	user, err := scimRequestUser(input, current.Active)
	if err != nil {
		writeSCIMError(writer, http.StatusBadRequest, err.Error())
		return
	}
	if err := server.scimGuardSharedAccount(writer, request, organizationID, current, user); err != nil {
		return
	}
	if user.Active && !current.Active {
		if err := server.scimRevokeSessions(request, organizationID, userID, "scim.user.session_revoke"); err != nil {
			writeSCIMError(writer, http.StatusInternalServerError, "the user could not be replaced")
			return
		}
	}
	replaced, err := server.database.SCIMReplaceUser(request.Context(), organizationID, userID, user,
		server.auditInput(request, "scim.user.replace", "user", userID, map[string]any{"email": user.Email, "active": user.Active}))
	if err != nil {
		if database.IsNotFound(err) {
			writeSCIMError(writer, http.StatusNotFound, "the user was not found")
			return
		}
		if errors.Is(err, database.ErrSCIMConflict) || database.IsConflict(err) {
			writeSCIMError(writer, http.StatusConflict, "an account with this email or external id already exists")
			return
		}
		writeSCIMError(writer, http.StatusInternalServerError, "the user could not be replaced")
		return
	}
	if !replaced.Active {
		if err := server.scimRevokeSessions(request, organizationID, userID, "scim.user.session_revoke"); err != nil {
			writeSCIMError(writer, http.StatusInternalServerError, "the user could not be replaced")
			return
		}
	}
	// Attribute sync and deactivation both move the billed seat count; keep
	// the Stripe quantity in step like the delete path does.
	server.syncSeatQuantity(request.Context(), organizationID)
	writeSCIM(writer, http.StatusOK, scimUserResponse(replaced))
}

// scimRevokeSessions kills every live session of the account through the
// provisioning store's deactivation transaction, which revokes the account's
// sessions in the same commit that records the deactivation.
func (server *Server) scimRevokeSessions(request *http.Request, organizationID, userID, action string) error {
	_, err := server.database.SCIMSetActive(request.Context(), organizationID, userID, false,
		server.auditInput(request, action, "user", userID, nil))
	return err
}

// scimGuardSharedAccount refuses account-wide changes a single tenant must
// not make alone. The users row is global: a person who belongs to several
// organizations (invites, SSO domain joins) must not have their email,
// external id, or active state rewritten — or their sessions revoked — at one
// tenant's provisioning whim while another tenant depends on the same
// account.
func (server *Server) scimGuardSharedAccount(writer http.ResponseWriter, request *http.Request, organizationID string, current, next database.SCIMUser) error {
	if strings.EqualFold(next.Email, current.Email) && next.ExternalID == current.ExternalID && next.Active == current.Active {
		return nil
	}
	memberships, err := server.database.Organizations(request.Context(), current.ID, database.NormalizePage(database.MaxPageLimit, 0))
	if err != nil {
		writeSCIMError(writer, http.StatusInternalServerError, "the user's memberships could not be read")
		return err
	}
	for _, membership := range memberships {
		if membership.ID != organizationID {
			writeSCIMError(writer, http.StatusConflict, "this account is a member of more than one organization; userName, externalId, and active are account-wide and cannot be changed by one organization's provisioning")
			return errors.New("account is shared across organizations")
		}
	}
	return nil
}

// handleSCIMUserPatch answers PATCH /Users/{id}. Only the "replace" operation
// is supported, on the whole attributes this server exposes — no filter paths,
// no add/remove. Anything else is a 400, which is more honest than silently
// dropping a change the idP believes it made. The active attribute follows
// the same session rules as a full replacement: deactivation revokes, and a
// re-enable never resurrects.
func (server *Server) handleSCIMUserPatch(writer http.ResponseWriter, request *http.Request) {
	var input scimPatchRequest
	if !decodeSCIM(writer, request, &input) {
		return
	}
	organizationID := principalFrom(request.Context()).OrganizationID
	userID := request.PathValue("userID")
	user, err := server.database.SCIMUserByID(request.Context(), organizationID, userID)
	if err != nil {
		if database.IsNotFound(err) {
			writeSCIMError(writer, http.StatusNotFound, "the user was not found")
			return
		}
		writeSCIMError(writer, http.StatusInternalServerError, "the user could not be read")
		return
	}
	if len(input.Operations) == 0 {
		writeSCIMError(writer, http.StatusBadRequest, "at least one patch operation is required")
		return
	}
	patched := database.SCIMUser{ExternalID: user.ExternalID, Email: user.Email, DisplayName: user.DisplayName, Active: user.Active}
	for _, operation := range input.Operations {
		if !strings.EqualFold(operation.Op, "replace") {
			writeSCIMError(writer, http.StatusBadRequest, fmt.Sprintf("patch operation %q is not supported; only \"replace\" is", operation.Op))
			return
		}
		value := []byte(operation.Value)
		switch strings.ToLower(strings.TrimSpace(operation.Path)) {
		case "active":
			if err := json.Unmarshal(value, &patched.Active); err != nil {
				writeSCIMError(writer, http.StatusBadRequest, "the active attribute takes a boolean value")
				return
			}
		case "displayname":
			if err := patchString(value, &patched.DisplayName); err != nil {
				writeSCIMError(writer, http.StatusBadRequest, "the displayName attribute takes a string value")
				return
			}
			if len(patched.DisplayName) > 200 {
				writeSCIMError(writer, http.StatusBadRequest, "displayName must be at most 200 characters")
				return
			}
		case "externalid":
			if err := patchString(value, &patched.ExternalID); err != nil {
				writeSCIMError(writer, http.StatusBadRequest, "the externalId attribute takes a string value")
				return
			}
			patched.ExternalID = strings.TrimSpace(patched.ExternalID)
			if len(patched.ExternalID) > 256 {
				writeSCIMError(writer, http.StatusBadRequest, "externalId must be at most 256 characters")
				return
			}
		case "username":
			address, err := patchEmail(value)
			if err != nil {
				writeSCIMError(writer, http.StatusBadRequest, err.Error())
				return
			}
			patched.Email = address
		default:
			writeSCIMError(writer, http.StatusBadRequest, fmt.Sprintf("patch path %q is not supported", operation.Path))
			return
		}
	}
	if err := server.scimGuardSharedAccount(writer, request, organizationID, user, patched); err != nil {
		return
	}
	if patched.Active && !user.Active {
		if err := server.scimRevokeSessions(request, organizationID, userID, "scim.user.session_revoke"); err != nil {
			writeSCIMError(writer, http.StatusInternalServerError, "the user could not be patched")
			return
		}
	}
	updated, err := server.database.SCIMReplaceUser(request.Context(), organizationID, userID, patched,
		server.auditInput(request, "scim.user.patch", "user", userID, map[string]any{"email": patched.Email, "active": patched.Active}))
	if err != nil {
		if database.IsNotFound(err) {
			writeSCIMError(writer, http.StatusNotFound, "the user was not found")
			return
		}
		if errors.Is(err, database.ErrSCIMConflict) || database.IsConflict(err) {
			writeSCIMError(writer, http.StatusConflict, "an account with this email or external id already exists")
			return
		}
		writeSCIMError(writer, http.StatusInternalServerError, "the user could not be patched")
		return
	}
	if !updated.Active {
		if err := server.scimRevokeSessions(request, organizationID, userID, "scim.user.session_revoke"); err != nil {
			writeSCIMError(writer, http.StatusInternalServerError, "the user could not be patched")
			return
		}
	}
	// Patches move the billed seat count as much as deletes do; keep the
	// Stripe quantity in step.
	server.syncSeatQuantity(request.Context(), organizationID)
	writeSCIM(writer, http.StatusOK, scimUserResponse(updated))
}

// patchString decodes one string-valued patch attribute.
func patchString(value []byte, target *string) error {
	var decoded string
	if err := json.Unmarshal(value, &decoded); err != nil {
		return err
	}
	*target = decoded
	return nil
}

// patchEmail decodes one email-valued patch attribute.
func patchEmail(value []byte) (string, error) {
	var decoded string
	if err := json.Unmarshal(value, &decoded); err != nil {
		return "", errors.New("the userName attribute takes a string value")
	}
	address, err := normalizeEmail(decoded)
	if err != nil {
		return "", errors.New("userName must be the user's email address")
	}
	return address, nil
}

// handleSCIMUserDelete answers DELETE /Users/{id}. In SCIM, deleting a user
// means deactivating the account: sessions die in the same transaction, but
// the row survives for the audit trail. An account shared with other
// organizations is not one tenant to disable.
func (server *Server) handleSCIMUserDelete(writer http.ResponseWriter, request *http.Request) {
	organizationID := principalFrom(request.Context()).OrganizationID
	userID := request.PathValue("userID")
	current, err := server.database.SCIMUserByID(request.Context(), organizationID, userID)
	if err != nil {
		if database.IsNotFound(err) {
			writeSCIMError(writer, http.StatusNotFound, "the user was not found")
			return
		}
		writeSCIMError(writer, http.StatusInternalServerError, "the user could not be read")
		return
	}
	if current.Active {
		next := current
		next.Active = false
		if err := server.scimGuardSharedAccount(writer, request, organizationID, current, next); err != nil {
			return
		}
	}
	if _, err := server.database.SCIMSetActive(request.Context(), organizationID, userID, false,
		server.auditInput(request, "scim.user.delete", "user", userID, nil)); err != nil {
		if database.IsNotFound(err) {
			writeSCIMError(writer, http.StatusNotFound, "the user was not found")
			return
		}
		writeSCIMError(writer, http.StatusInternalServerError, "the user could not be deactivated")
		return
	}
	// Deactivation frees the member's seat; keep the Stripe quantity in step.
	server.syncSeatQuantity(request.Context(), organizationID)
	writer.WriteHeader(http.StatusNoContent)
}

// handleSCIMServiceProviderConfig answers GET /ServiceProviderConfig: what
// this implementation supports, stated truthfully.
func (server *Server) handleSCIMServiceProviderConfig(writer http.ResponseWriter, request *http.Request) {
	writeSCIM(writer, http.StatusOK, map[string]any{
		"schemas":        []string{scimProviderSchema},
		"patch":          map[string]any{"supported": true},
		"bulk":           map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":         map[string]any{"supported": true, "maxResults": scimMaxPage},
		"changePassword": map[string]any{"supported": false},
		"sort":           map[string]any{"supported": false},
		"etag":           map[string]any{"supported": false},
		"authenticationSchemes": []map[string]any{{
			"name":        "API Key Bearer",
			"description": "An organization API key with the scim scope, sent as an HTTP bearer token",
			"specUri":     "https://datatracker.ietf.org/doc/html/rfc7644#section-2.1",
			"type":        "oauthbearertoken",
			"primary":     true,
		}},
	})
}

// handleSCIMResourceTypes answers GET /ResourceTypes.
func (server *Server) handleSCIMResourceTypes(writer http.ResponseWriter, request *http.Request) {
	writeSCIM(writer, http.StatusOK, map[string]any{
		"schemas":      []string{scimListResponseSchema},
		"totalResults": 1,
		"startIndex":   1,
		"itemsPerPage": 1,
		"Resources": []map[string]any{{
			"schemas":          []string{scimResourceTypeSchema},
			"id":               "User",
			"name":             "User",
			"endpoint":         "/v1/scim/v2/Users",
			"schema":           scimUserSchema,
			"schemaExtensions": []any{},
		}},
	})
}

// handleSCIMSchemas answers GET /Schemas with the User schema this
// implementation actually stores — no attributes it silently ignores.
func (server *Server) handleSCIMSchemas(writer http.ResponseWriter, request *http.Request) {
	writeSCIM(writer, http.StatusOK, map[string]any{
		"schemas":      []string{scimListResponseSchema},
		"totalResults": 1,
		"startIndex":   1,
		"itemsPerPage": 1,
		"Resources": []map[string]any{{
			"schemas":     []string{scimSchemaSchema},
			"id":          scimUserSchema,
			"name":        "User",
			"description": "A SHIFT user account, provisioned by the organization's identity provider.",
			"attributes": []map[string]any{
				{"name": "id", "type": "string", "multiValued": false, "required": true, "caseExact": true, "mutability": "readOnly", "returned": "always", "uniqueness": "global"},
				{"name": "externalId", "type": "string", "multiValued": false, "required": false, "caseExact": false, "mutability": "readWrite", "returned": "always", "uniqueness": "server"},
				{"name": "userName", "type": "string", "multiValued": false, "required": true, "caseExact": false, "mutability": "readWrite", "returned": "always", "uniqueness": "server"},
				{"name": "displayName", "type": "string", "multiValued": false, "required": false, "caseExact": false, "mutability": "readWrite", "returned": "always", "uniqueness": "none"},
				{"name": "active", "type": "boolean", "multiValued": false, "required": false, "mutability": "readWrite", "returned": "always"},
				{"name": "emails", "type": "complex", "multiValued": true, "required": false, "mutability": "readWrite", "returned": "always", "subAttributes": []map[string]any{
					{"name": "value", "type": "string", "multiValued": false, "required": false, "caseExact": false, "mutability": "readWrite", "returned": "always", "uniqueness": "none"},
					{"name": "primary", "type": "boolean", "multiValued": false, "required": false, "mutability": "readOnly", "returned": "always"},
				}},
				{"name": "meta", "type": "complex", "multiValued": false, "required": false, "mutability": "readOnly", "returned": "default", "subAttributes": []map[string]any{
					{"name": "resourceType", "type": "string", "multiValued": false, "required": false, "mutability": "readOnly", "returned": "default"},
					{"name": "location", "type": "string", "multiValued": false, "required": false, "mutability": "readOnly", "returned": "default"},
				}},
			},
		}},
	})
}
