package plugin

import (
	dContainer "github.com/docker/docker/api/types/container"
)

// Compose label keys. Docker Compose stamps every container it creates
// with these, and they survive a `compose up -d` recreate (same project
// + service ⇒ same values), which is exactly the stable identity the
// stable-lease client-id (#219) and the deterministic MAC (#218) both
// need. Defined here rather than imported because the plugin otherwise
// has no Compose dependency.
const (
	composeProjectLabel = "com.docker.compose.project"
	composeServiceLabel = "com.docker.compose.service"
	// composeNumberLabel is the per-replica index Compose stamps on each
	// container of a service (1, 2, 3 for `--scale svc=3`). It is the
	// piece that keeps replicas of one service from hashing to the same
	// seed: project+service alone are identical across replicas, so the
	// number is what makes the seed replica-unique while staying stable
	// across recreate (replica N keeps its number).
	composeNumberLabel = "com.docker.compose.container-number"
)

// containerIdentity is the best-effort stable identity of the container
// an endpoint is being created for, resolved once at create time (see
// initialContainerIdentity). Every field is best-effort: a non-Compose
// `docker run` container has empty Compose fields, and an anonymous
// container (`--rm` with no name) can even have an empty Name. The seed
// logic (stableLeaseSeed) degrades through these gracefully.
type containerIdentity struct {
	// Hostname is the container's configured hostname, consumed by the
	// option-12 DHCP hint (initialContainerIdentity → DISCOVER). Not used
	// as a stable seed: it defaults to the short container ID, which is
	// NOT stable across recreate.
	Hostname string
	// Name is the Docker container name (e.g. "/myproj-web-1"). Stable
	// across recreate for named and Compose-managed containers.
	Name string
	// ComposeProject / ComposeService / ComposeNumber come from the
	// Compose labels above. Project+Service+Number together form a
	// replica-unique identity that's stable across `compose up -d` even
	// if the container name changes. Number may be empty on older
	// Compose releases that don't stamp it; the seed logic falls back to
	// the container name (which also carries the replica index) in that
	// case.
	ComposeProject string
	ComposeService string
	ComposeNumber  string
}

// containerIdentityFromInspect extracts the stable-identity fields from a
// Docker ContainerInspect result. Shared by initialContainerIdentity (the
// CreateEndpoint poll) and the persistent dhcpManager (which inspects the
// same container at Join / recovery), so both derive the identical seed
// for the same container — the recompute is deterministic, no plumbing of
// the chosen client-id between stages required.
func containerIdentityFromInspect(ctr dContainer.InspectResponse) containerIdentity {
	var id containerIdentity
	// Name lives on the embedded *ContainerJSONBase, which a real
	// ContainerInspect always populates but a hand-built fixture may not.
	if ctr.ContainerJSONBase != nil {
		id.Name = ctr.Name
	}
	if ctr.Config != nil {
		id.Hostname = ctr.Config.Hostname
		id.ComposeProject = ctr.Config.Labels[composeProjectLabel]
		id.ComposeService = ctr.Config.Labels[composeServiceLabel]
		id.ComposeNumber = ctr.Config.Labels[composeNumberLabel]
	}
	return id
}
