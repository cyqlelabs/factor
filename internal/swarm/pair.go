package swarm

import (
	"context"

	"github.com/cyqlelabs/factor/internal/tools"
)

func (s *Service) invite(_ context.Context, _ tools.ToolContext, _ string) *tools.Result {
	return tools.Errorf("pairing needs the rendezvous, which this build does not reach yet")
}

func (s *Service) join(_ context.Context, _ tools.ToolContext) *tools.Result {
	return tools.Errorf("pairing needs the rendezvous, which this build does not reach yet")
}

func (s *Service) removeMember(_ context.Context, _ string) *tools.Result {
	return tools.Errorf("removing a member needs the rendezvous, which this build does not reach yet")
}
