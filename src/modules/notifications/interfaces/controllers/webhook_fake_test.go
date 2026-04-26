//go:build server

package controllers_test

import (
	"context"

	notiApp "github.com/cuadra/cuadra-core/src/modules/notifications/app"
)

// noopProcess satisfies WebhookProcessor for the signature-only test. It
// records the calls so tests can assert the controller forwarded inputs as
// expected.
type noopProcess struct {
	Calls []notiApp.ProcessWebhookInput
}

func newNoopProcess() *noopProcess { return &noopProcess{} }

func (p *noopProcess) Execute(_ context.Context, in notiApp.ProcessWebhookInput) (*notiApp.ProcessWebhookOutput, error) {
	p.Calls = append(p.Calls, in)
	return &notiApp.ProcessWebhookOutput{}, nil
}
