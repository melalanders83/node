package command_run

import (
	"github.com/mysterium/node/communication"
	"github.com/mysterium/node/communication/nats_dialog"
	"github.com/mysterium/node/communication/nats_discovery"
	"github.com/mysterium/node/openvpn"
	"github.com/mysterium/node/server"
	dto_discovery "github.com/mysterium/node/service_discovery/dto"
	"os"
)

func NewCommand(vpnMiddlewares ...openvpn.ManagementMiddleware) *CommandRun {
	nats_discovery.Bootstrap()
	openvpn.Bootstrap()

	return &CommandRun{
		Output:      os.Stdout,
		OutputError: os.Stderr,

		MysteriumClient: server.NewClient(),
		CommunicationClientFactory: func(identity dto_discovery.Identity) communication.Client {
			return nats_dialog.NewClient(identity)
		},

		vpnMiddlewares: vpnMiddlewares,
	}
}
