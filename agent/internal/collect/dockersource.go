package collect

import (
	"context"
	"strings"

	"snugbox.dev/snugbox/agent/internal/docker"
	"snugbox.dev/snugbox/agent/internal/model"
)

// DockerSource names containers through the Docker Engine API.
type DockerSource struct {
	Client *docker.Client
}

// Containers lists every container and inspects each one for its limits and
// restart count. It asks Docker every time, so ids don't matter to it.
func (d DockerSource) Containers(ctx context.Context, _ []string) (map[string]ContainerMeta, error) {
	if _, err := d.Client.Version(ctx); err != nil {
		return nil, err
	}
	list, err := d.Client.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]ContainerMeta, len(list))
	for _, s := range list {
		if !docker.ValidID(s.ID) {
			continue
		}
		meta := ContainerMeta{Image: s.Image, Compose: composeOf(s.Labels)}
		if len(s.Names) > 0 {
			meta.Name = strings.TrimPrefix(s.Names[0], "/")
		}
		if ct, err := d.Client.Inspect(ctx, s.ID); err == nil {
			meta.Restarts = ct.RestartCount
			meta.Started, meta.Restarting = ct.State.StartedAt, ct.State.Status == "restarting"
			if ct.HostConfig.MemoryReservation > 0 {
				meta.Reservation = uint64(ct.HostConfig.MemoryReservation)
			}
		}
		out[s.ID] = meta
	}
	return out, nil
}

func composeOf(labels map[string]string) model.Compose {
	return model.Compose{
		Project:    labels["com.docker.compose.project"],
		Service:    labels["com.docker.compose.service"],
		WorkingDir: labels["com.docker.compose.project.working_dir"],
	}
}
