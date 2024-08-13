package connector

import (
	"context"
	"strings"
	"text/template"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
	"maunium.net/go/mautrix/bridgev2"

	"maunium.net/go/mautrix-whatsapp/pkg/msgconv"
)

type WhatsAppConnector struct {
	Bridge      *bridgev2.Bridge
	Config      *WhatsAppConfig
	DeviceStore *sqlstore.Container
	MsgConv     *msgconv.MessageConverter
}

var _ bridgev2.NetworkConnector = (*WhatsAppConnector)(nil)
var _ bridgev2.MaxFileSizeingNetwork = (*WhatsAppConnector)(nil)

func NewConnector() *WhatsAppConnector {
	return &WhatsAppConnector{
		Config: &WhatsAppConfig{},
	}
}

func (wa *WhatsAppConnector) SetMaxFileSize(maxSize int64) {
	println("SetMaxFileSize unimplemented")
}

var WhatsAppGeneralCaps = &bridgev2.NetworkGeneralCapabilities{
	DisappearingMessages: true,
	AggressiveUpdateInfo: false,
}

func (wa *WhatsAppConnector) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return WhatsAppGeneralCaps
}

func (wa *WhatsAppConnector) GetName() bridgev2.BridgeName {
	return bridgev2.BridgeName{
		DisplayName:      "WhatsApp",
		NetworkURL:       "https://whatsapp.com",
		NetworkIcon:      "mxc://maunium.net/NeXNQarUbrlYBiPCpprYsRqr",
		NetworkID:        "whatsapp",
		BeeperBridgeType: "whatsapp",
		DefaultPort:      29318,
	}
}

func (wa *WhatsAppConnector) Init(bridge *bridgev2.Bridge) {
	var err error
	wa.Config.displaynameTemplate, err = template.New("displayname").Parse(wa.Config.Bridge.DisplaynameTemplate)
	if err != nil {
		// TODO return error or do this later?
		panic(err)
	}
	wa.Bridge = bridge
	wa.MsgConv = msgconv.New(bridge)

	wa.DeviceStore = sqlstore.NewWithDB(
		bridge.DB.RawDB,
		bridge.DB.Dialect.String(),
		waLog.Zerolog(bridge.Log.With().Str("db_section", "whatsmeow").Logger()),
	)

	store.DeviceProps.Os = proto.String(wa.Config.WhatsApp.OSName)
	store.DeviceProps.RequireFullSync = proto.Bool(wa.Config.Bridge.HistorySync.RequestFullSync)
	if fsc := wa.Config.Bridge.HistorySync.FullSyncConfig; fsc.DaysLimit > 0 && fsc.SizeLimit > 0 && fsc.StorageQuota > 0 {
		store.DeviceProps.HistorySyncConfig = &waCompanionReg.DeviceProps_HistorySyncConfig{
			FullSyncDaysLimit:   proto.Uint32(fsc.DaysLimit),
			FullSyncSizeMbLimit: proto.Uint32(fsc.SizeLimit),
			StorageQuotaMb:      proto.Uint32(fsc.StorageQuota),
		}
	}
	platformID, ok := waCompanionReg.DeviceProps_PlatformType_value[strings.ToUpper(wa.Config.WhatsApp.BrowserName)]
	if ok {
		store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_PlatformType(platformID).Enum()
	}
}

func (wa *WhatsAppConnector) Start(ctx context.Context) error {
	err := wa.DeviceStore.Upgrade()
	if err != nil {
		return bridgev2.DBUpgradeError{Err: err, Section: "whatsmeow"}
	}

	ver, err := whatsmeow.GetLatestVersion(nil)
	if err != nil {
		wa.Bridge.Log.Err(err).Msg("Failed to get latest WhatsApp web version number")
	} else {
		wa.Bridge.Log.Debug().
			Stringer("hardcoded_version", store.GetWAVersion()).
			Stringer("latest_version", *ver).
			Msg("Got latest WhatsApp web version number")
		store.SetWAVersion(*ver)
	}
	return nil
}

func (wa *WhatsAppConnector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	loginMetadata := login.Metadata.(*UserLoginMetadata)

	jid := types.JID{
		Device: loginMetadata.WADeviceID,
		Server: types.DefaultUserServer,
		User:   string(login.ID),
	}

	device, err := wa.DeviceStore.GetDevice(jid)

	if err != nil {
		return err
	}

	w := &WhatsAppClient{
		Main:      wa,
		UserLogin: login,
		Device:    device,
	}

	w.MakeNewClient()

	err = w.Client.Connect()

	if err != nil {
		login.Log.Err(err).Msg("Error connecting to WhatsApp")
	}

	login.Client = w
	return nil
}
