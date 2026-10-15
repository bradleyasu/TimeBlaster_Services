package wifi

import (
	"context"
	"fmt"
)

// PowerOff shuts the system down.
//
// The fixed argv is the whole of the safety story here: the request carries no
// fields, so nothing the daemon sends can change what runs. systemctl queues the
// job and returns at once, so the reply still reaches the daemon before services
// start stopping.
//
// poweroff rather than halt: with the bootloader's POWER_OFF_ON_HALT set the
// board then cuts USB power once it is finished, and the Nano's display going
// dark is the signal that it is safe to unplug.
func (s *Server) PowerOff(ctx context.Context) error {
	s.log.Warn("powering off at the daemon's request")
	if _, err := s.runner.Run(ctx, "systemctl", "poweroff"); err != nil {
		return fmt.Errorf("wifi: powering off: %w", err)
	}
	return nil
}
