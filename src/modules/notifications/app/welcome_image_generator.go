package app

import "context"

// WelcomeImageGenerator genera un banner PNG con el PIN embebido y lo
// sube al bucket público de R2. Retorna la URL pública. La interfaz
// permite inyectar un mock en tests sin levantar R2.
type WelcomeImageGenerator interface {
	Generate(ctx context.Context, pin string) (string, error)
}
