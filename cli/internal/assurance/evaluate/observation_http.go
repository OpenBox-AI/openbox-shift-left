package evaluate

import (
	"net/http"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/observation"
)

func newObservationHTTPClient() *http.Client { return observation.NewHTTPClient() }
