// Package ndpcmd provides the commands for the ndp utility.
package ndpcmd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/mdlayher/ndp"
	"golang.org/x/sync/errgroup"
)

var (
	errPrefixOp = errors.New("flag '-p' is only valid for router advertisement operation")
	errTargetOp = errors.New("flag '-t' is only valid for neighbor solicitation operation")
)

// Run runs the ndp utility.
func Run(ctx context.Context, c *ndp.Conn, ifi *net.Interface, op string, target, prefix net.IP) error {
	if op != "ns" && target != nil {
		return errTargetOp
	}
	if op != "ra" && prefix != nil {
		return errPrefixOp
	}

	switch op {
	// listen is the default when no op is specified.
	case "listen", "":
		return listen(ctx, c)
	case "ns":
		return sendNS(ctx, c, ifi.HardwareAddr, target)
	case "ra":
		if prefix == nil || prefix.Equal(net.IPv6zero) {
			return errors.New("flag '-p' is required for router advertisement operation")
		}

		return doRA(ctx, c, ifi.HardwareAddr, prefix)
	case "rs":
		return sendRS(ctx, c, ifi.HardwareAddr)
	default:
		return fmt.Errorf("unrecognized operation: %q", op)
	}
}

func listen(ctx context.Context, c *ndp.Conn) error {
	ll := log.New(os.Stderr, "ndp listen> ", 0)
	ll.Println("listening for messages")

	// Also listen for router solicitations from other hosts, even though we
	// will never reply to them.
	if err := c.JoinGroup(net.IPv6linklocalallrouters); err != nil {
		return err
	}

	// No filtering, print all messages.
	if err := receiveLoop(ctx, c, ll, nil, nil); err != nil {
		return fmt.Errorf("failed to read message: %v", err)
	}

	return nil
}

func sendNS(ctx context.Context, c *ndp.Conn, addr net.HardwareAddr, target net.IP) error {
	ll := log.New(os.Stderr, "ndp ns> ", 0)

	ll.Printf("neighbor solicitation:\n    - source link-layer address: %s", addr.String())

	// Always multicast the message to the target's solicited-node multicast
	// group as if we have no knowledge of its MAC address.
	snm, err := ndp.SolicitedNodeMulticast(target)
	if err != nil {
		return fmt.Errorf("failed to determine solicited-node multicast address: %v", err)
	}

	m := &ndp.NeighborSolicitation{
		TargetAddress: target,
		Options: []ndp.Option{
			&ndp.LinkLayerAddress{
				Direction: ndp.Source,
				Addr:      addr,
			},
		},
	}

	// Expect neighbor advertisement messages with the correct target address.
	check := func(m ndp.Message) bool {
		na, ok := m.(*ndp.NeighborAdvertisement)
		if !ok {
			return false
		}

		return na.TargetAddress.Equal(target)
	}

	if err := sendReceiveLoop(ctx, c, ll, m, snm, check); err != nil {
		if err == context.Canceled {
			return err
		}

		return fmt.Errorf("failed to send neighbor solicitation: %v", err)
	}

	return nil
}

func doRA(ctx context.Context, c *ndp.Conn, addr net.HardwareAddr, prefix net.IP) error {
	ll := log.New(os.Stderr, "ndp ra> ", 0)

	ll.Printf("advertising prefix %s/64 for SLAAC", prefix)

	// This tool is mostly meant for testing so hardcode a bunch of values.
	m := &ndp.RouterAdvertisement{
		CurrentHopLimit:           64,
		RouterSelectionPreference: ndp.Medium,
		RouterLifetime:            30 * time.Second,
		Options: []ndp.Option{
			&ndp.PrefixInformation{
				PrefixLength:                   64,
				AutonomousAddressConfiguration: true,
				ValidLifetime:                  60 * time.Second,
				PreferredLifetime:              30 * time.Second,
				Prefix:                         prefix,
			},
			&ndp.LinkLayerAddress{
				Direction: ndp.Source,
				Addr:      addr,
			},
		},
	}

	// Expect any router solicitation message.
	check := func(m ndp.Message) bool {
		_, ok := m.(*ndp.RouterSolicitation)
		return ok
	}

	// Trigger an RA whenever an RS is received.
	rsC := make(chan struct{})
	recv := func(ll *log.Logger, msg ndp.Message, from net.IP) {
		printMessage(ll, m, from)
		rsC <- struct{}{}
	}

	// We are now a "router".
	if err := c.JoinGroup(net.IPv6linklocalallrouters); err != nil {
		return fmt.Errorf("failed to join multicast group: %v", err)
	}

	var eg errgroup.Group
	eg.Go(func() error {
		// Send messages until cancelation or error.
		for {
			if err := c.WriteTo(m, nil, net.IPv6linklocalallnodes); err != nil {
				return fmt.Errorf("failed to send router advertisement: %v", err)
			}

			select {
			case <-ctx.Done():
				return nil
			// Trigger RA at regular intervals or on demand.
			case <-time.After(10 * time.Second):
			case <-rsC:
			}
		}
	})

	if err := receiveLoop(ctx, c, ll, check, recv); err != nil {
		return fmt.Errorf("failed to receive router solicitations: %v", err)
	}

	return eg.Wait()
}

func sendRS(ctx context.Context, c *ndp.Conn, addr net.HardwareAddr) error {
	ll := log.New(os.Stderr, "ndp rs> ", 0)

	ll.Printf("router solicitation:\n    - source link-layer address: %s", addr.String())

	m := &ndp.RouterSolicitation{
		Options: []ndp.Option{
			&ndp.LinkLayerAddress{
				Direction: ndp.Source,
				Addr:      addr,
			},
		},
	}

	// Expect any router advertisement message.
	check := func(m ndp.Message) bool {
		_, ok := m.(*ndp.RouterAdvertisement)
		return ok
	}

	if err := sendReceiveLoop(ctx, c, ll, m, net.IPv6linklocalallrouters, check); err != nil {
		if err == context.Canceled {
			return err
		}

		return fmt.Errorf("failed to send router solicitation: %v", err)
	}

	return nil
}
