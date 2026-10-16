package domain

import "context"

// PaymentEventResolver reads provider-owned data after webhook signature
// verification. Notification fields are not trusted as payment evidence.
type PaymentEventResolver interface {
	Resolve(ctx context.Context, resourceType, resourceID string) (*Event, error)
}
