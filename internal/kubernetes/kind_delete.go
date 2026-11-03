package kubernetes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/mbianchidev/porto/internal/runtimes"
)

type kindRemovalGrant struct {
	ctx                          context.Context
	owner, runtime, controlPlane string
}

func (p *ClusterProvisioner) kindRemovalEnvironment(ctx context.Context, request ClusterRequest, environment []string) ([]string, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	nodes := kindNodeNames(request)
	if len(nodes) == 0 {
		return nil, nil, fmt.Errorf("managed kind removal requires an explicit control-plane identity")
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return nil, nil, fmt.Errorf("create managed kind deletion authorization: %w", err)
	}
	token := hex.EncodeToString(random)
	p.ownershipMu.Lock()
	if p.kindRemovals == nil {
		p.kindRemovals = map[string]kindRemovalGrant{}
	}
	p.kindRemovals[token] = kindRemovalGrant{ctx: ctx, owner: request.Name, runtime: clusterRuntimeName(request), controlPlane: nodes[0]}
	p.ownershipMu.Unlock()
	revoke := func() { p.ownershipMu.Lock(); delete(p.kindRemovals, token); p.ownershipMu.Unlock() }
	environment = replaceEnvironmentVariable(environment, "DOCKER_CUSTOM_HEADERS", runtimes.ManagedContainerRemovalHeader+"="+token)
	return environment, revoke, nil
}

func (p *ClusterProvisioner) kindRemovalAuthorized(ctx context.Context, request ClusterRequest, containerName string) bool {
	token := runtimes.ManagedContainerRemovalToken(ctx)
	if len(token) != 64 || ctx.Err() != nil {
		return false
	}
	p.ownershipMu.Lock()
	grant, ok := p.kindRemovals[token]
	p.ownershipMu.Unlock()
	return ok && grant.ctx.Err() == nil && grant.owner == request.Name && grant.runtime == clusterRuntimeName(request) && grant.controlPlane == containerName
}
