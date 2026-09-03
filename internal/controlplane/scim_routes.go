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
// create, and on replace the protocol's read of a full replacement.
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

// handleSCIMUsersList answers GET /Users with a ListResponse. Filtering uses
// the database layer's parser — attribute and operator names come from fixed
// whitelists and values only ever bind as parameters.
func (server *Server) handleSCIMUsersList(writer http.ResponseWriter, request *http.Request) {
	filter, err := database.ParseSCIMFilter(request.URL.Query().Get("filter"))
	if err != nil {
		writeSCIMError(writer, http.StatusBadRequest, err.Error())
		return
	}
	users, err := server.database.SCIMUsers(request.Context(), principalFrom(request.Context()).OrganizationID, filter)
	if err != nil {
		writeSCIMError(writer, http.StatusInternalServerError, "the user list could not be read")
		return
	}
	startIndex := int(parseMetadataInt(request.URL.Query().Get("startIndex"), 1))
	count := int(parseMetadataInt(request.URL.Query().Get("count"), 100))
	if count > scimMaxPage {
		count = scimMaxPage
	}
	begin := startIndex - 1
	if begin < 0 || begin > len(users) {
		begin = len(users)
	}
	end := begin + count
	if end > len(users) {
		end = len(users)
	}
	resources := make([]map[string]any, 0, end-begin)
	for _, user := range users[begin:end] {
		resources = append(resources, scimUserResponse(user))
	}
	writeSCIM(writer, http.StatusOK, map[string]any{
		"schemas":      []string{scimListResponseSchema},
		"totalResults": len(users),
		"startIndex":   begin + 1,
		"itemsPerPage": len(resources),
		"Resources":    resources,
	})
}

// handleSCIMUserCreate answers POST /Users.
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
		writeSCIMError(writer, http.StatusInternalServerError, "the user could not be created")
		return
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
// idP-controlled fields.
func (server *Server) handleSCIMUserReplace(writer http.ResponseWriter, request *http.Request) {
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
	userID := request.PathValue("userID")
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
	writeSCIM(writer, http.StatusOK, scimUserResponse(replaced))
}

// handleSCIMUserPatch answers PATCH /Users/{id}. Only the "replace" operation
// is supported, on the whole attributes this server exposes — no filter paths,
// no add/remove. Anything else is a 400, which is more honest than silently
// dropping a change the idP believes it made.
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
		case "externalid":
			if err := patchString(value, &patched.ExternalID); err != nil {
				writeSCIMError(writer, http.StatusBadRequest, "the externalId attribute takes a string value")
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
	updated, err := server.database.SCIMReplaceUser(request.Context(), organizationID, userID, patched,
		server.auditInput(request, "scim.user.patch", "user", userID, map[string]any{"email": patched.Email, "active": patched.Active}))
	if err != nil {
		if database.IsNotFound(err) {
			writeSCIMError(writer, http.StatusNotFound, "the user was not found")
			return
		}
		writeSCIMError(writer, http.StatusInternalServerError, "the user could not be patched")
		return
	}
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
// the row survives for the audit trail.
func (server *Server) handleSCIMUserDelete(writer http.ResponseWriter, request *http.Request) {
	organizationID := principalFrom(request.Context()).OrganizationID
	userID := request.PathValue("userID")
	if _, err := server.database.SCIMSetActive(request.Context(), organizationID, userID, false,
		server.auditInput(request, "scim.user.delete", "user", userID, nil)); err != nil {
		if database.IsNotFound(err) {
			writeSCIMError(writer, http.StatusNotFound, "the user was not found")
			return
		}
		writeSCIMError(writer, http.StatusInternalServerError, "the user could not be deactivated")
		return
	}
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
			"type":        "httpbasic",
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
