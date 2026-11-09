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

// RestartServices restarts the Timeblaster daemon and this helper.
//
// Like PowerOff, the request carries no fields: the units are fixed here, so
// nothing the daemon sends can name anything else. --no-block queues the
// restart and returns at once, so the reply reaches the daemon before systemd
// stops either service -- including this one.
func (s *Server) RestartServices(ctx context.Context) error {
	s.log.Warn("restarting the Timeblaster services at the daemon's request")
	if _, err := s.runner.Run(ctx, "systemctl", "--no-block", "restart",
		"timeblaster.service", "timeblaster-wifi.service"); err != nil {
		return fmt.Errorf("wifi: restarting the services: %w", err)
	}
	return nil
}
