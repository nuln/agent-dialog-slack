// Package slack integrates the Slack messaging platform.
package slack

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	agent "github.com/nuln/agent-core"
	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

func init() {
	agent.RegisterPluginConfigSpec(agent.PluginConfigSpec{
		PluginName:  "slack",
		PluginType:  "dialog",
		Description: "Slack bot messaging platform integration (Socket Mode)",
		AuthType:    agent.AuthTypeToken,
		Fields: []agent.ConfigField{
			{Key: "bot_token", EnvVar: "SLACK_BOT_TOKEN", Description: "Slack Bot OAuth token (xoxb-...)", Required: true, Type: agent.ConfigFieldSecret},
			{Key: "app_token", EnvVar: "SLACK_APP_TOKEN", Description: "Slack App-level token for Socket Mode (xapp-...)", Required: true, Type: agent.ConfigFieldSecret},
		},
	})

	agent.RegisterDialog("slack", func(opts map[string]any) (agent.Dialog, error) {
		return New(opts)
	})
}

type replyContext struct {
	channelID string
	userID    string
	threadTS  string
}

// SlackDialog implements agent.Dialog for Slack bots via Socket Mode.
type SlackDialog struct {
	mu       sync.RWMutex
	botToken string
	appToken string
	client   *slackgo.Client
	sm       *socketmode.Client
	handler  agent.MessageHandler
	status   agent.DialogInstanceStatus
	cancel   context.CancelFunc
}

// New creates a SlackDialog from options.
func New(opts map[string]any) (*SlackDialog, error) {
	botToken, _ := opts["bot_token"].(string)
	if botToken == "" {
		botToken = os.Getenv("SLACK_BOT_TOKEN")
	}
	appToken, _ := opts["app_token"].(string)
	if appToken == "" {
		appToken = os.Getenv("SLACK_APP_TOKEN")
	}
	if botToken == "" || appToken == "" {
		return nil, fmt.Errorf("slack: SLACK_BOT_TOKEN and SLACK_APP_TOKEN are required")
	}
	return &SlackDialog{botToken: botToken, appToken: appToken}, nil
}

func (d *SlackDialog) Name() string { return "slack" }

func (d *SlackDialog) Start(handler agent.MessageHandler) error {
	d.handler = handler
	d.client = slackgo.New(d.botToken, slackgo.OptionAppLevelToken(d.appToken))
	d.sm = socketmode.New(d.client)

	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel

	go func() {
		if err := d.sm.RunContext(ctx); err != nil && ctx.Err() == nil {
			slog.Error("slack socket mode error", "err", err)
		}
	}()

	go d.handleEvents(ctx)

	d.mu.Lock()
	d.status = agent.DialogInstanceStatus{
		ID:          "slack",
		Status:      "connected",
		InboundAt:   time.Now().UnixMilli(),
		Description: "Slack Socket Mode connected",
	}
	d.mu.Unlock()
	slog.Info("slack: bot started")
	return nil
}

func (d *SlackDialog) handleEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-d.sm.Events:
			if !ok {
				return
			}
			if evt.Type != socketmode.EventTypeEventsAPI {
				d.sm.Ack(*evt.Request)
				continue
			}
			eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
			if !ok {
				d.sm.Ack(*evt.Request)
				continue
			}
			d.sm.Ack(*evt.Request)
			if eventsAPIEvent.Type != slackevents.CallbackEvent {
				continue
			}
			if eventsAPIEvent.InnerEvent.Type != "message" {
				continue
			}
			msgEvt, ok := eventsAPIEvent.InnerEvent.Data.(*slackevents.MessageEvent)
			if !ok {
				continue
			}
			if msgEvt.BotID != "" {
				continue
			}
			rctx := replyContext{channelID: msgEvt.Channel, userID: msgEvt.User, threadTS: msgEvt.ThreadTimeStamp}
			sessionKey := fmt.Sprintf("slack:%s:%s", msgEvt.Channel, msgEvt.User)
			msg := &agent.Message{
				SessionKey: sessionKey,
				UserID:     msgEvt.User,
				Content:    msgEvt.Text,
				ReplyCtx:   rctx,
			}
			if d.handler != nil {
				d.handler(d, msg)
			}
		}
	}
}

func (d *SlackDialog) Reply(ctx context.Context, replyCtx any, content string) error {
	return d.Send(ctx, replyCtx, content)
}

func (d *SlackDialog) Send(ctx context.Context, replyCtx any, content string) error {
	rctx, ok := replyCtx.(replyContext)
	if !ok {
		return fmt.Errorf("slack: invalid reply context")
	}
	opts := []slackgo.MsgOption{slackgo.MsgOptionText(content, false)}
	if rctx.threadTS != "" {
		opts = append(opts, slackgo.MsgOptionTS(rctx.threadTS))
	}
	_, _, err := d.client.PostMessageContext(ctx, rctx.channelID, opts...)
	if err != nil {
		return fmt.Errorf("slack: post message: %w", err)
	}
	d.mu.Lock()
	d.status.InboundAt = time.Now().UnixMilli()
	d.mu.Unlock()
	return nil
}

func (d *SlackDialog) Stop() error {
	if d.cancel != nil {
		d.cancel()
	}
	return nil
}

func (d *SlackDialog) Reload(opts map[string]any) error {
	_ = d.Stop()
	nd, err := New(opts)
	if err != nil {
		return err
	}
	d.botToken = nd.botToken
	d.appToken = nd.appToken
	d.client = nil
	d.sm = nil
	return d.Start(d.handler)
}
