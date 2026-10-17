package modelwire

import "fmt"

// HTTPError exposes only the endpoint status, never its body or credentials.
type HTTPError struct{ Status int }

func (e *HTTPError) Error() string { return fmt.Sprintf("model endpoint returned HTTP %d", e.Status) }
