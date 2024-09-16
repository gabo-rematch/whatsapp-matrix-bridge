package connector

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/ptr"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"maunium.net/go/mautrix-whatsapp/pkg/waid"
)

func (wa *WhatsAppClient) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (wrapped *bridgev2.ChatInfo, err error) {
	portalJID, err := waid.ParsePortalID(portal.ID)
	if err != nil {
		return nil, err
	}
	switch portalJID.Server {
	case types.DefaultUserServer:
		wrapped = wa.wrapDMInfo(portalJID)
	case types.BroadcastServer:
		if portalJID == types.StatusBroadcastJID {
			wrapped = wa.wrapStatusBroadcastInfo()
		} else {
			return nil, fmt.Errorf("broadcast list bridging is currently not supported")
		}
	case types.GroupServer:
		info, err := wa.Client.GetGroupInfo(portalJID)
		if err != nil {
			return nil, err
		}
		wrapped = wa.wrapGroupInfo(info)
	case types.NewsletterServer:
		info, err := wa.Client.GetNewsletterInfo(portalJID)
		if err != nil {
			return nil, err
		}
		wrapped = wa.wrapNewsletterInfo(info)
	default:
		return nil, fmt.Errorf("unsupported server %s", portalJID.Server)
	}
	var conv *waHistorySync.Conversation
	applyHistoryInfo(wrapped, conv)
	wa.applyChatSettings(ctx, portalJID, wrapped)
	return wrapped, nil
}

func updateDisappearingTimerSetAt(ts int64) bridgev2.ExtraUpdater[*bridgev2.Portal] {
	return func(_ context.Context, portal *bridgev2.Portal) bool {
		meta := portal.Metadata.(*waid.PortalMetadata)
		if meta.DisappearingTimerSetAt != ts {
			meta.DisappearingTimerSetAt = ts
			return true
		}
		return false
	}
}

func (wa *WhatsAppClient) applyChatSettings(ctx context.Context, chatID types.JID, info *bridgev2.ChatInfo) {
	chat, err := wa.Client.Store.ChatSettings.GetChatSettings(chatID)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to get chat settings")
		return
	}
	info.UserLocal = &bridgev2.UserLocalPortalInfo{
		MutedUntil: ptr.Ptr(chat.MutedUntil),
	}
	if chat.Pinned {
		info.UserLocal.Tag = ptr.Ptr(event.RoomTagFavourite)
	} else if chat.Archived {
		info.UserLocal.Tag = ptr.Ptr(event.RoomTagLowPriority)
	}
}

func applyHistoryInfo(info *bridgev2.ChatInfo, conv *waHistorySync.Conversation) {
	if conv == nil {
		return
	}
	info.CanBackfill = true
	info.UserLocal = &bridgev2.UserLocalPortalInfo{
		MutedUntil: ptr.Ptr(time.Unix(int64(conv.GetMuteEndTime()), 0)),
	}
	if conv.GetPinned() > 0 {
		info.UserLocal.Tag = ptr.Ptr(event.RoomTagFavourite)
	} else if conv.GetArchived() {
		info.UserLocal.Tag = ptr.Ptr(event.RoomTagLowPriority)
	}
	if conv.GetEphemeralExpiration() > 0 {
		info.Disappear = &database.DisappearingSetting{
			Type:  database.DisappearingTypeAfterRead,
			Timer: time.Duration(conv.GetEphemeralExpiration()) * time.Second,
		}
		info.ExtraUpdates = bridgev2.MergeExtraUpdaters(info.ExtraUpdates, updateDisappearingTimerSetAt(conv.GetEphemeralSettingTimestamp()))
	}
}

const StatusBroadcastTopic = "WhatsApp status updates from your contacts"
const StatusBroadcastName = "WhatsApp Status Broadcast"
const BroadcastTopic = "WhatsApp broadcast list"
const UnnamedBroadcastName = "Unnamed broadcast list"
const PrivateChatTopic = "WhatsApp private chat"

func (wa *WhatsAppClient) wrapDMInfo(jid types.JID) *bridgev2.ChatInfo {
	info := &bridgev2.ChatInfo{
		Topic: ptr.Ptr(PrivateChatTopic),
		Members: &bridgev2.ChatMemberList{
			IsFull:           true,
			TotalMemberCount: 2,
			OtherUserID:      waid.MakeUserID(jid),
			MemberMap: map[networkid.UserID]bridgev2.ChatMember{
				waid.MakeUserID(jid):    {EventSender: wa.makeEventSender(jid)},
				waid.MakeUserID(wa.JID): {EventSender: wa.makeEventSender(wa.JID)},
			},
			PowerLevels: nil,
		},
		Type: ptr.Ptr(database.RoomTypeDM),
	}
	if jid == wa.JID.ToNonAD() {
		// For chats with self, force-split the members so the user's own ghost is always in the room.
		info.Members.MemberMap = map[networkid.UserID]bridgev2.ChatMember{
			waid.MakeUserID(jid): {EventSender: bridgev2.EventSender{Sender: waid.MakeUserID(jid)}},
			"":                   {EventSender: bridgev2.EventSender{IsFromMe: true}},
		}
	}
	return info
}

func (wa *WhatsAppClient) wrapStatusBroadcastInfo() *bridgev2.ChatInfo {
	return &bridgev2.ChatInfo{
		Name:  ptr.Ptr(StatusBroadcastName),
		Topic: ptr.Ptr(StatusBroadcastTopic),
		Members: &bridgev2.ChatMemberList{
			IsFull: false,
			MemberMap: map[networkid.UserID]bridgev2.ChatMember{
				waid.MakeUserID(wa.JID): {EventSender: wa.makeEventSender(wa.JID)},
			},
		},
		Type:        ptr.Ptr(database.RoomTypeDefault),
		CanBackfill: false,
	}
}

const (
	nobodyPL     = 99
	superAdminPL = 75
	adminPL      = 50
	defaultPL    = 0
)

func (wa *WhatsAppClient) wrapGroupInfo(info *types.GroupInfo) *bridgev2.ChatInfo {
	sendEventPL := defaultPL
	if info.IsAnnounce {
		sendEventPL = adminPL
	}
	metaChangePL := defaultPL
	if info.IsLocked {
		metaChangePL = adminPL
	}
	wrapped := &bridgev2.ChatInfo{
		Name:  ptr.Ptr(info.Name),
		Topic: ptr.Ptr(info.Topic),
		Members: &bridgev2.ChatMemberList{
			IsFull:           !info.IsIncognito,
			TotalMemberCount: len(info.Participants),
			MemberMap:        make(map[networkid.UserID]bridgev2.ChatMember, len(info.Participants)),
			PowerLevels: &bridgev2.PowerLevelOverrides{
				EventsDefault: &sendEventPL,
				StateDefault:  ptr.Ptr(nobodyPL),
				Ban:           ptr.Ptr(nobodyPL),
				// TODO allow invites if bridge config says to allow them, or maybe if relay mode is enabled?
				Events: map[event.Type]int{
					event.StateRoomName:   metaChangePL,
					event.StateRoomAvatar: metaChangePL,
					event.StateTopic:      metaChangePL,
					event.EventReaction:   defaultPL,
					event.EventRedaction:  defaultPL,
					// TODO always allow poll responses
				},
			},
		},
		Disappear: &database.DisappearingSetting{
			Type:  database.DisappearingTypeAfterRead,
			Timer: time.Duration(info.DisappearingTimer) * time.Second,
		},
	}
	for _, pcp := range info.Participants {
		if pcp.JID.Server != types.DefaultUserServer {
			continue
		}
		member := bridgev2.ChatMember{
			EventSender: wa.makeEventSender(pcp.JID),
			Membership:  event.MembershipJoin,
		}
		if pcp.IsSuperAdmin {
			member.PowerLevel = ptr.Ptr(superAdminPL)
		} else if pcp.IsAdmin {
			member.PowerLevel = ptr.Ptr(adminPL)
		} else {
			member.PowerLevel = ptr.Ptr(defaultPL)
		}
		wrapped.Members.MemberMap[waid.MakeUserID(pcp.JID)] = member
	}

	if !info.LinkedParentJID.IsEmpty() {
		wrapped.ParentID = ptr.Ptr(waid.MakePortalID(info.LinkedParentJID))
	}
	if info.IsParent {
		wrapped.Type = ptr.Ptr(database.RoomTypeSpace)
	} else {
		wrapped.Type = ptr.Ptr(database.RoomTypeDefault)
	}
	return wrapped
}

func (wa *WhatsAppClient) wrapNewsletterInfo(info *types.NewsletterMetadata) *bridgev2.ChatInfo {
	ownPowerLevel := defaultPL
	var mutedUntil *time.Time
	if info.ViewerMeta != nil {
		switch info.ViewerMeta.Role {
		case types.NewsletterRoleAdmin:
			ownPowerLevel = adminPL
		case types.NewsletterRoleOwner:
			ownPowerLevel = superAdminPL
		}
		switch info.ViewerMeta.Mute {
		case types.NewsletterMuteOn:
			mutedUntil = &event.MutedForever
		case types.NewsletterMuteOff:
			mutedUntil = &bridgev2.Unmuted
		}
	}
	avatar := &bridgev2.Avatar{}
	if info.ThreadMeta.Picture != nil {
		avatar.ID = networkid.AvatarID(info.ThreadMeta.Picture.ID)
		avatar.Get = func(ctx context.Context) ([]byte, error) {
			return wa.Client.DownloadMediaWithPath(info.ThreadMeta.Picture.DirectPath, nil, nil, nil, 0, "", "")
		}
	} else if info.ThreadMeta.Preview.ID != "" {
		avatar.ID = networkid.AvatarID(info.ThreadMeta.Preview.ID)
		avatar.Get = func(ctx context.Context) ([]byte, error) {
			meta, err := wa.Client.GetNewsletterInfo(info.ID)
			if err != nil {
				return nil, fmt.Errorf("failed to fetch full res avatar info: %w", err)
			} else if meta.ThreadMeta.Picture == nil {
				return nil, fmt.Errorf("full res avatar info is missing")
			}
			return wa.Client.DownloadMediaWithPath(meta.ThreadMeta.Picture.DirectPath, nil, nil, nil, 0, "", "")
		}
	} else {
		avatar.ID = "remove"
		avatar.Remove = true
	}
	return &bridgev2.ChatInfo{
		Name:   ptr.Ptr(info.ThreadMeta.Name.Text),
		Topic:  ptr.Ptr(info.ThreadMeta.Description.Text),
		Avatar: avatar,
		UserLocal: &bridgev2.UserLocalPortalInfo{
			MutedUntil: mutedUntil,
		},
		Members: &bridgev2.ChatMemberList{
			TotalMemberCount: info.ThreadMeta.SubscriberCount,
			MemberMap: map[networkid.UserID]bridgev2.ChatMember{
				waid.MakeUserID(wa.JID): {
					EventSender: wa.makeEventSender(wa.JID),
					PowerLevel:  &ownPowerLevel,
				},
			},
			PowerLevels: &bridgev2.PowerLevelOverrides{
				EventsDefault: ptr.Ptr(adminPL),
				StateDefault:  ptr.Ptr(nobodyPL),
				Ban:           ptr.Ptr(nobodyPL),
				Events: map[event.Type]int{
					event.StateRoomName:   adminPL,
					event.StateRoomAvatar: adminPL,
					event.StateTopic:      adminPL,
					event.EventReaction:   defaultPL,
					event.EventRedaction:  defaultPL,
					// TODO always allow poll responses
				},
			},
		},
		Type: ptr.Ptr(database.RoomTypeDefault),
	}
}
