package ios

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/dmdmdm-nz/zeroconf"
	log "github.com/sirupsen/logrus"
)

const RSD_PORT int = 58783

func FindRemotedServiceAddresses(ctx context.Context) ([]string, error) {
	entries := make(chan *zeroconf.ServiceEntry)

	deviceAddrs := make(map[string]bool)

	go func(results <-chan *zeroconf.ServiceEntry) {
		for entry := range results {
			iface, err := net.InterfaceByIndex(entry.ReceivedIfIndex)
			if err != nil {
				log.WithField("index", entry.ReceivedIfIndex).
					Error("Failed to get interface by index:", err.Error())
				continue
			}

			addr := fmt.Sprintf("%s%%%s", entry.AddrIPv6[0].String(), iface.Name)
			if _, exists := deviceAddrs[addr]; !exists {
				deviceAddrs[addr] = true
			}
		}
	}(entries)

	ctx, cancel := context.WithTimeout(ctx, time.Second*3)
	defer cancel()

	// Discover all remoted services
	err := zeroconf.Browse(
		ctx,
		"_remoted._tcp",
		"local.",
		entries,
		zeroconf.SelectIPTraffic(zeroconf.IPv6))
	if err != nil {
		log.Fatalln("Failed to browse:", err.Error())
		return nil, err
	}

	<-ctx.Done()

	addrs := make([]string, 0)

	for addr := range deviceAddrs {
		addrs = append(addrs, addr)
	}

	return addrs, nil
}

func TryGetRsdUdid(ctx context.Context, addr string) (string, error) {
	s, err := NewWithAddrPort(addr, RSD_PORT)
	if err != nil {
		return "", err
	}
	defer s.Close()

	h, err := s.Handshake()
	if err != nil {
		return "", err
	}

	return h.Udid, nil
}
