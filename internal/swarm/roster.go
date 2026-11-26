package swarm

import "github.com/cyqlelabs/factor/internal/swarm/wire"

// The roster and its members are wire types: the rendezvous checks the same
// signatures the agents do.
type (
	Roster       = wire.Roster
	RosterMember = wire.RosterMember
)

const (
	RoleMember = wire.RoleMember
	RoleGuest  = wire.RoleGuest
)

// NewSwarmID makes the random id a new swarm is known by.
func NewSwarmID() string { return wire.NewSwarmID() }

// Genesis is the first roster of a new swarm: id alone.
func Genesis(id *Identity, swarm, home string) *Roster { return wire.Genesis(id, swarm, home) }
