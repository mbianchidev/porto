package runtimes

import "context"

const ManagedContainerRemovalHeader = "X-Porto-Managed-Container-Removal"

type managedContainerRemovalKey struct{}

func WithManagedContainerRemovalToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, managedContainerRemovalKey{}, token)
}

func ManagedContainerRemovalToken(ctx context.Context) string {
	token, _ := ctx.Value(managedContainerRemovalKey{}).(string)
	return token
}
