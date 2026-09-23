package backend

import (
	"errors"
	"net/http"
	"strings"
)

// CreateConflict classifies a 409 from Create. The backend sends no
// reason_code on these, only a message string (agent-registration-
// identity.service.ts:198, 217, 291-293, 549-551), so the match is on stable
// substrings, case-insensitive.
type CreateConflict int

const (
	// None means err is not a 409 create conflict at all: nil, a non-
	// *APIError, or an *APIError with a different status code.
	None CreateConflict = iota
	// IdPNotInitialized: the org has no active identity-provider generation
	// yet. An operator must run bootstrap-legacy-did-authority --apply.
	IdPNotInitialized
	// ExternalIdP: the org's active provider is Okta/Entra, not OpenBox.
	// Primary match (:198, checked before the generate/openbox branch) is
	// what an Okta/Entra org actually returns for source_type=openbox;
	// secondary is the generate-mode-on-external-provider message.
	ExternalIdP
	// IdPChangedRetry: the active generation changed mid-registration. The
	// caller may retry.
	IdPChangedRetry
	// Other409 is a recognized-status, unrecognized-message create conflict
	// (e.g. the link-mode eligibility 409), not one of the above.
	Other409
)

func (c CreateConflict) String() string {
	switch c {
	case None:
		return "None"
	case IdPNotInitialized:
		return "IdPNotInitialized"
	case ExternalIdP:
		return "ExternalIdP"
	case IdPChangedRetry:
		return "IdPChangedRetry"
	case Other409:
		return "Other409"
	default:
		return "CreateConflict(unknown)"
	}
}

// ClassifyCreateConflict reads a Create error's *APIError (409 only) and
// classifies its message. Any other status code, or an error that is not an
// *APIError at all (including nil), is None.
func ClassifyCreateConflict(err error) CreateConflict {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict {
		return None
	}
	body := strings.ToLower(apiErr.Body)
	switch {
	case strings.Contains(body, "must be initialized"):
		return IdPNotInitialized
	case strings.Contains(body, "does not belong to the active identity provider"):
		return ExternalIdP
	case strings.Contains(body, "new identities must be created in the active external provider"):
		return ExternalIdP
	case strings.Contains(body, "identity provider changed during agent registration"):
		return IdPChangedRetry
	default:
		return Other409
	}
}
