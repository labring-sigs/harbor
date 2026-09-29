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

// ErrQuotaNotFound is returned when a project quota cannot be resolved.
type ErrQuotaNotFound struct {
	ProjectID int64
	QuotaID   int64
}

func (e *ErrQuotaNotFound) Error() string {
	if e.QuotaID > 0 {
		return fmt.Sprintf("harbor quota %d not found", e.QuotaID)
	}
	return fmt.Sprintf("harbor quota for project %d not found", e.ProjectID)
}

// ErrAPIError is returned when the Harbor API returns a non-2xx status
type ErrAPIError struct {
	StatusCode int
	Body       string
}

func (e *ErrAPIError) Error() string {
	return fmt.Sprintf("harbor API error: status=%d body=%s", e.StatusCode, e.Body)
}
