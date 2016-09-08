// Copyright © 2016 The Things Network
// Use of this source code is governed by the MIT license that can be found in the LICENSE file.

package networkserver

import (
	"fmt"
	"strings"
	"time"

	pb_broker "github.com/TheThingsNetwork/ttn/api/broker"
	pb_handler "github.com/TheThingsNetwork/ttn/api/handler"
	pb "github.com/TheThingsNetwork/ttn/api/networkserver"
	pb_protocol "github.com/TheThingsNetwork/ttn/api/protocol"
	pb_lorawan "github.com/TheThingsNetwork/ttn/api/protocol/lorawan"
	"github.com/TheThingsNetwork/ttn/core"
	"github.com/TheThingsNetwork/ttn/core/fcnt"
	"github.com/TheThingsNetwork/ttn/core/networkserver/device"
	"github.com/TheThingsNetwork/ttn/core/types"
	"github.com/TheThingsNetwork/ttn/utils/random"
	"github.com/brocaar/lorawan"
	"gopkg.in/redis.v3"
)

// NetworkServer implements LoRaWAN-specific functionality for TTN
type NetworkServer interface {
	core.ComponentInterface
	core.ManagementInterface

	UsePrefix(prefix types.DevAddrPrefix, usage []string) error
	GetPrefixesFor(requiredUsages ...string) []types.DevAddrPrefix

	HandleGetDevices(*pb.DevicesRequest) (*pb.DevicesResponse, error)
	HandlePrepareActivation(*pb_broker.DeduplicatedDeviceActivationRequest) (*pb_broker.DeduplicatedDeviceActivationRequest, error)
	HandleActivate(*pb_handler.DeviceActivationResponse) (*pb_handler.DeviceActivationResponse, error)
	HandleUplink(*pb_broker.DeduplicatedUplinkMessage) (*pb_broker.DeduplicatedUplinkMessage, error)
	HandleDownlink(*pb_broker.DownlinkMessage) (*pb_broker.DownlinkMessage, error)
}

// NewRedisNetworkServer creates a new Redis-backed NetworkServer
func NewRedisNetworkServer(client *redis.Client, netID int) NetworkServer {
	ns := &networkServer{
		devices:  device.NewRedisDeviceStore(client),
		prefixes: map[types.DevAddrPrefix][]string{},
	}
	ns.netID = [3]byte{byte(netID >> 16), byte(netID >> 8), byte(netID)}
	return ns
}

type networkServer struct {
	*core.Component
	devices  device.Store
	netID    [3]byte
	prefixes map[types.DevAddrPrefix][]string
}

func (n *networkServer) UsePrefix(prefix types.DevAddrPrefix, usage []string) error {
	if prefix.Length < 7 {
		return core.NewErrInvalidArgument("Prefix", "invalid length")
	}
	if prefix.DevAddr[0]>>1 != n.netID[2] {
		return core.NewErrInvalidArgument("Prefix", "invalid netID")
	}
	n.prefixes[prefix] = usage
	return nil
}

func (n *networkServer) GetPrefixesFor(requiredUsages ...string) []types.DevAddrPrefix {
	var suitablePrefixes []types.DevAddrPrefix
	for prefix, offeredUsages := range n.prefixes {
		matches := 0
		for _, requiredUsage := range requiredUsages {
			for _, offeredUsage := range offeredUsages {
				if offeredUsage == requiredUsage {
					matches++
				}
			}
		}
		if matches == len(requiredUsages) {
			suitablePrefixes = append(suitablePrefixes, prefix)
		}
	}
	return suitablePrefixes
}

func (n *networkServer) Init(c *core.Component) error {
	n.Component = c
	err := n.Component.UpdateTokenKey()
	if err != nil {
		return err
	}
	n.Component.SetStatus(core.StatusHealthy)
	return nil
}

func (n *networkServer) HandleGetDevices(req *pb.DevicesRequest) (*pb.DevicesResponse, error) {
	devices, err := n.devices.GetWithAddress(*req.DevAddr)
	if err != nil {
		return nil, err
	}

	// Return all devices with DevAddr with FCnt <= fCnt or Security off

	res := &pb.DevicesResponse{
		Results: make([]*pb_lorawan.Device, 0, len(devices)),
	}

	for _, device := range devices {
		fullFCnt := fcnt.GetFull(device.FCntUp, uint16(req.FCnt))
		dev := &pb_lorawan.Device{
			AppEui:           &device.AppEUI,
			AppId:            device.AppID,
			DevEui:           &device.DevEUI,
			DevId:            device.DevID,
			NwkSKey:          &device.NwkSKey,
			FCntUp:           device.FCntUp,
			Uses32BitFCnt:    device.Options.Uses32BitFCnt,
			DisableFCntCheck: device.Options.DisableFCntCheck,
		}
		if device.Options.DisableFCntCheck {
			res.Results = append(res.Results, dev)
			continue
		}
		if device.FCntUp <= req.FCnt {
			res.Results = append(res.Results, dev)
			continue
		} else if device.Options.Uses32BitFCnt && device.FCntUp <= fullFCnt {
			res.Results = append(res.Results, dev)
			continue
		}
	}

	return res, nil
}

func (n *networkServer) getDevAddr(constraints ...string) (types.DevAddr, error) {
	// Instantiate a new random source
	random := random.New()

	// Generate random DevAddr bytes
	var devAddr types.DevAddr
	copy(devAddr[:], random.Bytes(4))

	// Get a random prefix that matches the constraints
	prefixes := n.GetPrefixesFor(constraints...)
	if len(prefixes) == 0 {
		return types.DevAddr{}, core.NewErrNotFound(fmt.Sprintf("DevAddr prefix with constraints %v", constraints))
	}

	// Select a prefix
	prefix := prefixes[random.Intn(len(prefixes))]

	// Apply the prefix
	devAddr = devAddr.WithPrefix(prefix)

	return devAddr, nil
}

func (n *networkServer) HandlePrepareActivation(activation *pb_broker.DeduplicatedDeviceActivationRequest) (*pb_broker.DeduplicatedDeviceActivationRequest, error) {
	if activation.AppEui == nil || activation.DevEui == nil {
		return nil, core.NewErrInvalidArgument("Activation", "missing AppEUI or DevEUI")
	}
	dev, err := n.devices.Get(*activation.AppEui, *activation.DevEui)
	if err != nil {
		return nil, err
	}
	activation.AppId = dev.AppID
	activation.DevId = dev.DevID

	// Get activation constraints (for DevAddr prefix selection)
	activationConstraints := strings.Split(dev.Options.ActivationConstraints, ",")
	if len(activationConstraints) == 1 && activationConstraints[0] == "" {
		activationConstraints = []string{}
	}
	activationConstraints = append(activationConstraints, "otaa")

	// Build activation metadata if not present
	if meta := activation.GetActivationMetadata(); meta == nil {
		activation.ActivationMetadata = &pb_protocol.ActivationMetadata{}
	}
	// Build lorawan metadata if not present
	if lorawan := activation.ActivationMetadata.GetLorawan(); lorawan == nil {
		return nil, core.NewErrInvalidArgument("Activation", "missing LoRaWAN metadata")
	}

	// Build response template if not present
	if pld := activation.GetResponseTemplate(); pld == nil {
		return nil, core.NewErrInvalidArgument("Activation", "missing response template")
	}
	lorawanMeta := activation.ActivationMetadata.GetLorawan()

	// Get a random device address
	devAddr, err := n.getDevAddr(activationConstraints...)
	if err != nil {
		return nil, err
	}

	// Set the DevAddr in the Activation Metadata
	lorawanMeta.DevAddr = &devAddr

	// Build JoinAccept Payload
	phy := lorawan.PHYPayload{
		MHDR: lorawan.MHDR{
			MType: lorawan.JoinAccept,
			Major: lorawan.LoRaWANR1,
		},
		MACPayload: &lorawan.JoinAcceptPayload{
			NetID:      n.netID,
			DLSettings: lorawan.DLSettings{RX2DataRate: uint8(lorawanMeta.Rx2Dr), RX1DROffset: uint8(lorawanMeta.Rx1DrOffset)},
			RXDelay:    uint8(lorawanMeta.RxDelay),
			DevAddr:    lorawan.DevAddr(devAddr),
		},
	}
	if len(lorawanMeta.CfList) == 5 {
		var cfList lorawan.CFList
		for i, cfListItem := range lorawanMeta.CfList {
			cfList[i] = uint32(cfListItem)
		}
		phy.MACPayload.(*lorawan.JoinAcceptPayload).CFList = &cfList
	}

	// Set the Payload
	phyBytes, err := phy.MarshalBinary()
	if err != nil {
		return nil, err
	}
	activation.ResponseTemplate.Payload = phyBytes

	return activation, nil
}

func (n *networkServer) HandleActivate(activation *pb_handler.DeviceActivationResponse) (*pb_handler.DeviceActivationResponse, error) {
	meta := activation.GetActivationMetadata()
	if meta == nil {
		return nil, core.NewErrInvalidArgument("Activation", "missing ActivationMetadata")
	}
	lorawan := meta.GetLorawan()
	if lorawan == nil {
		return nil, core.NewErrInvalidArgument("Activation", "missing LoRaWAN ActivationMetadata")
	}
	err := n.devices.Activate(*lorawan.AppEui, *lorawan.DevEui, *lorawan.DevAddr, *lorawan.NwkSKey)
	if err != nil {
		return nil, err
	}
	return activation, nil
}

func (n *networkServer) HandleUplink(message *pb_broker.DeduplicatedUplinkMessage) (*pb_broker.DeduplicatedUplinkMessage, error) {
	// Get Device
	dev, err := n.devices.Get(*message.AppEui, *message.DevEui)
	if err != nil {
		return nil, err
	}

	// Unmarshal LoRaWAN Payload
	var phyPayload lorawan.PHYPayload
	err = phyPayload.UnmarshalBinary(message.Payload)
	if err != nil {
		return nil, err
	}
	macPayload, ok := phyPayload.MACPayload.(*lorawan.MACPayload)
	if !ok {
		return nil, core.NewErrInvalidArgument("Uplink", "does not contain a MAC payload")
	}

	// Update FCntUp (from metadata if possible, because only 16lsb are marshaled in FHDR)
	if lorawan := message.GetProtocolMetadata().GetLorawan(); lorawan != nil {
		dev.FCntUp = lorawan.FCnt
	} else {
		dev.FCntUp = macPayload.FHDR.FCnt
	}
	dev.LastSeen = time.Now()
	err = n.devices.Set(dev, "f_cnt_up", "last_seen")
	if err != nil {
		return nil, err
	}

	// Prepare Downlink
	if message.ResponseTemplate == nil {
		return message, nil
	}
	message.ResponseTemplate.AppEui = message.AppEui
	message.ResponseTemplate.DevEui = message.DevEui
	message.ResponseTemplate.AppId = message.AppId
	message.ResponseTemplate.DevId = message.DevId

	// Add Full FCnt (avoiding nil pointer panics)
	if option := message.ResponseTemplate.DownlinkOption; option != nil {
		if protocol := option.ProtocolConfig; protocol != nil {
			if lorawan := protocol.GetLorawan(); lorawan != nil {
				lorawan.FCnt = dev.FCntDown
			}
		}
	}

	phy := lorawan.PHYPayload{
		MHDR: lorawan.MHDR{
			MType: lorawan.UnconfirmedDataDown,
			Major: lorawan.LoRaWANR1,
		},
		MACPayload: &lorawan.MACPayload{
			FHDR: lorawan.FHDR{
				DevAddr: macPayload.FHDR.DevAddr,
				FCtrl: lorawan.FCtrl{
					ACK: phyPayload.MHDR.MType == lorawan.ConfirmedDataUp,
				},
				FCnt: dev.FCntDown,
			},
		},
	}
	phyBytes, err := phy.MarshalBinary()
	if err != nil {
		return nil, err
	}

	// TODO: Maybe we need to add MAC commands on downlink

	message.ResponseTemplate.Payload = phyBytes

	return message, nil
}

func (n *networkServer) HandleDownlink(message *pb_broker.DownlinkMessage) (*pb_broker.DownlinkMessage, error) {
	// Get Device
	dev, err := n.devices.Get(*message.AppEui, *message.DevEui)
	if err != nil {
		return nil, err
	}

	if dev.AppID != message.AppId || dev.DevID != message.DevId {
		return nil, core.NewErrInvalidArgument("Downlink", "AppID and DevID do not match AppEUI and DevEUI")
	}

	// Unmarshal LoRaWAN Payload
	var phyPayload lorawan.PHYPayload
	err = phyPayload.UnmarshalBinary(message.Payload)
	if err != nil {
		return nil, err
	}
	macPayload, ok := phyPayload.MACPayload.(*lorawan.MACPayload)
	if !ok {
		return nil, core.NewErrInvalidArgument("Downlink", "does not contain a MAC payload")
	}

	// Set DevAddr
	macPayload.FHDR.DevAddr = lorawan.DevAddr(dev.DevAddr)

	// FIRST set and THEN increment FCntDown
	// TODO: For confirmed downlink, FCntDown should be incremented AFTER ACK
	macPayload.FHDR.FCnt = dev.FCntDown
	dev.FCntDown++
	err = n.devices.Set(dev, "f_cnt_down")
	if err != nil {
		return nil, err
	}

	// TODO: Maybe we need to add MAC commands on downlink

	// Sign MIC
	phyPayload.SetMIC(lorawan.AES128Key(dev.NwkSKey))

	// Update message
	bytes, err := phyPayload.MarshalBinary()
	if err != nil {
		return nil, err
	}
	message.Payload = bytes

	return message, nil
}
