package harbor

import "fmt"

// ErrNotFound is returned when a requested resource is not found
type ErrNotFound struct {
	Resource string
	ID       interface{}
}

func (e *ErrNotFound) Error() string {
	return fmt.Sprintf("harbor %s not found: %v", e.Resource, e.ID)
}

// ErrAPIError is returned when the Harbor API returns a non-2xx status
type ErrAPIError struct {
	StatusCode int
	Body       string
}

func (e *ErrAPIError) Error() string {
	return fmt.Sprintf("harbor API error: status=%d body=%s", e.StatusCode, e.Body)
}
