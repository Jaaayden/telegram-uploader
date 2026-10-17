package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	coreapp "github.com/Jaaayden/telegram-uploader/internal/app"
	"github.com/Jaaayden/telegram-uploader/internal/credentials"
	"github.com/Jaaayden/telegram-uploader/internal/diagnostics"
	tgtransport "github.com/Jaaayden/telegram-uploader/internal/telegram"
)

type connectionRequest struct {
	appID             int
	apiHash           string
	botToken          string
	proxyEnabled      bool
	proxyAddress      string
	proxyUsername     string
	proxyPassword     string
	uploadConcurrency int
}

func uploadConcurrencyOptions() []string {
	return []string{
		uploadConcurrencyCompatibilityLabel,
		uploadConcurrencyBalancedLabel,
		uploadConcurrencyFastLabel,
	}
}

func uploadConcurrencyForOption(option string) (int, bool) {
	switch option {
	case uploadConcurrencyCompatibilityLabel:
		return tgtransport.UploadConcurrencyCompatibility, true
	case uploadConcurrencyBalancedLabel:
		return tgtransport.UploadConcurrencyBalanced, true
	case uploadConcurrencyFastLabel:
		return tgtransport.UploadConcurrencyFast, true
	default:
		return 0, false
	}
}

func uploadConcurrencyOptionFor(value int) string {
	switch value {
	case tgtransport.UploadConcurrencyCompatibility:
		return uploadConcurrencyCompatibilityLabel
	case tgtransport.UploadConcurrencyFast:
		return uploadConcurrencyFastLabel
	case tgtransport.UploadConcurrencyBalanced:
		fallthrough
	default:
		return uploadConcurrencyBalancedLabel
	}
}

func normalizeUploadConcurrency(value int) int {
	if normalized, ok := uploadConcurrencyForOption(uploadConcurrencyOptionFor(value)); ok {
		return normalized
	}
	return tgtransport.DefaultUploadConcurrency
}

func (u *window) autoConnectIfConfigured() {
	if u.settingsLoadErr != nil {
		u.connection.SetText("设置文件读取失败")
		u.updateReadinessHint()
		return
	}
	if u.credentialLoadErr != nil {
		u.connection.SetText("无法读取系统凭证")
		u.updateReadinessHint()
		return
	}
	if !u.hasCompleteConnectionConfig() {
		u.updateReadinessHint()
		return
	}
	u.connectWithMode(true)
}

func (u *window) hasCompleteConnectionConfig() bool {
	appID, err := strconv.Atoi(strings.TrimSpace(u.apiID.Text))
	return err == nil && appID > 0 && strings.TrimSpace(u.apiHash.Text) != "" && strings.TrimSpace(u.botToken.Text) != ""
}

func (u *window) canAttemptAutomaticConnection() bool {
	u.clientMu.RLock()
	hasApplied := u.hasAppliedConnection
	u.clientMu.RUnlock()
	return hasApplied || u.hasCompleteConnectionConfig()
}

func (u *window) handleUploadConcurrencyChanged(option string) {
	concurrency, ok := uploadConcurrencyForOption(option)
	if !ok {
		return
	}
	previous := normalizeUploadConcurrency(u.settingsSnapshot().UploadConcurrency)
	if err := u.updateSettings(func(settings *coreapp.Settings) {
		settings.UploadConcurrency = concurrency
	}); err != nil {
		if u.uploadConcurrency != nil {
			u.uploadConcurrency.OnChanged = nil
			u.uploadConcurrency.SetSelected(uploadConcurrencyOptionFor(previous))
			u.uploadConcurrency.OnChanged = u.handleUploadConcurrencyChanged
		}
		u.showError(err)
		return
	}
	u.clientMu.Lock()
	if u.hasAppliedConnection {
		u.appliedConnection.uploadConcurrency = concurrency
	}
	if u.pendingReconnect != nil {
		u.pendingReconnect.uploadConcurrency = concurrency
	}
	client := u.client
	u.clientMu.Unlock()
	if client != nil {
		concurrency = client.SetUploadConcurrency(concurrency)
	}
	u.operationLabel.SetText(fmt.Sprintf("上传并发已设置为 %d 路；当前视频不受影响，将从下一个视频开始生效", concurrency))
}

func (u *window) connect() {
	u.connectWithMode(false)
}

func (u *window) connectWithMode(automatic bool) {
	if !automatic && u.queueRunning() {
		u.showError(errors.New("上传进行中不能更换 Telegram 凭证；上传并发仍可随时调整"))
		return
	}
	if !automatic {
		u.resetConnectionRetry()
	}
	var request connectionRequest
	hasApplied := false
	if automatic {
		u.clientMu.RLock()
		request = u.appliedConnection
		hasApplied = u.hasAppliedConnection
		u.clientMu.RUnlock()
	}
	if !hasApplied {
		var err error
		request, err = u.connectionRequestFromFields()
		if err != nil {
			if automatic && u.hasCompleteConnectionConfig() {
				u.connectionConfigErr = err
				u.connection.SetText("连接配置错误，请检查设置")
				u.updateReadinessHint()
			} else if !automatic {
				u.showError(err)
			}
			return
		}
	}
	u.connectWithRequest(request, automatic)
}

func (u *window) connectionRequestFromFields() (connectionRequest, error) {
	appID, err := strconv.Atoi(strings.TrimSpace(u.apiID.Text))
	if err != nil || appID <= 0 {
		return connectionRequest{}, errors.New("API ID 必须是正整数")
	}
	apiHash := strings.TrimSpace(u.apiHash.Text)
	botToken := strings.TrimSpace(u.botToken.Text)
	if apiHash == "" || botToken == "" {
		return connectionRequest{}, errors.New("API Hash 和 Bot Token 不能为空")
	}
	request := connectionRequest{
		appID:             appID,
		apiHash:           apiHash,
		botToken:          botToken,
		proxyEnabled:      u.proxyEnabled.Checked,
		proxyAddress:      strings.TrimSpace(u.proxyAddress.Text),
		proxyUsername:     strings.TrimSpace(u.proxyUsername.Text),
		proxyPassword:     u.proxyPassword.Text,
		uploadConcurrency: u.selectedUploadConcurrency(),
	}
	if request.proxyEnabled && request.proxyAddress == "" {
		return connectionRequest{}, errors.New("已启用 SOCKS5 代理，但代理地址为空")
	}
	return request, nil
}

func (u *window) connectWithRequest(request connectionRequest, automatic bool) {
	if !automatic && u.queueRunning() {
		u.showError(errors.New("上传进行中不能更换 Telegram 凭证；上传并发仍可随时调整"))
		return
	}
	u.connectionConfigErr = nil

	u.clientMu.Lock()
	if u.client != nil {
		if automatic || u.connecting {
			u.clientMu.Unlock()
			return
		}
		pending := request
		u.pendingReconnect = &pending
		u.reconnectRequested = true
		u.connecting = true
		cancel := u.clientCancel
		u.clientMu.Unlock()
		u.connected = false
		u.identity = tgtransport.Identity{}
		u.controller.SetGateway(nil)
		u.connectButton.Disable()
		u.bindButton.Disable()
		u.limit.SetText("当前上传上限：未获取")
		u.connection.SetText("正在断开旧连接并应用新设置……")
		u.updateActionAvailability()
		if cancel != nil {
			cancel()
		}
		return
	}
	if u.connecting || u.isClosed() {
		u.clientMu.Unlock()
		return
	}
	u.connecting = true
	u.clientGeneration++
	generation := u.clientGeneration
	if automatic && !u.hasAppliedConnection {
		u.appliedConnection = request
		u.hasAppliedConnection = true
	}
	u.clientMu.Unlock()
	u.stopConnectionRetryTimer()
	_ = diagnostics.Logf(
		"telegram_connect_requested generation=%d automatic=%t proxy=%t upload_concurrency=%d",
		generation,
		automatic,
		request.proxyEnabled,
		normalizeUploadConcurrency(request.uploadConcurrency),
	)
	u.connectButton.Disable()
	if automatic {
		u.connection.SetText("正在自动连接……")
	} else {
		u.connection.SetText("正在保存凭据并连接……")
	}
	u.updateActionAvailability()
	u.startConnectionWorker(func() {
		u.startConnectionAttempt(request, automatic, generation)
	})
}

func (u *window) startConnectionAttempt(request connectionRequest, automatic bool, generation uint64) {
	if u.connectionStarter != nil {
		u.connectionStarter(request, automatic, generation)
		return
	}
	u.connectAsync(request, automatic, generation)
}

func (u *window) connectAsync(request connectionRequest, automatic bool, generation uint64) {
	if !automatic {
		if err := u.updateSettings(func(settings *coreapp.Settings) {
			settings.APIID = request.appID
			settings.ProxyEnabled = request.proxyEnabled
			settings.ProxyAddress = request.proxyAddress
			settings.ProxyUsername = request.proxyUsername
			settings.UploadConcurrency = normalizeUploadConcurrency(request.uploadConcurrency)
		}); err != nil {
			u.handleConnectionFailure(err, automatic, false, generation)
			return
		}
		if err := u.secrets.SetAPIHash(request.apiHash); err != nil {
			u.handleConnectionFailure(err, automatic, false, generation)
			return
		}
		if err := u.secrets.SetBotToken(request.botToken); err != nil {
			u.handleConnectionFailure(err, automatic, false, generation)
			return
		}
		if request.proxyPassword == "" {
			if err := u.secrets.DeleteProxyPassword(); err != nil && !errors.Is(err, credentials.ErrNotFound) {
				u.handleConnectionFailure(err, automatic, false, generation)
				return
			}
		} else if err := u.secrets.SetProxyPassword(request.proxyPassword); err != nil {
			u.handleConnectionFailure(err, automatic, false, generation)
			return
		}
		request.uploadConcurrency = normalizeUploadConcurrency(u.settingsSnapshot().UploadConcurrency)
		u.clientMu.Lock()
		if generation == u.clientGeneration && !u.isClosed() {
			u.appliedConnection = request
			u.hasAppliedConnection = true
		}
		u.clientMu.Unlock()
		u.doUI(func() {
			u.settingsLoadErr = nil
			u.credentialLoadErr = nil
			u.updateReadinessHint()
		})
	}
	request.uploadConcurrency = normalizeUploadConcurrency(u.settingsSnapshot().UploadConcurrency)

	var proxyConfig *tgtransport.ProxyConfig
	if request.proxyEnabled && request.proxyAddress != "" {
		proxyConfig = &tgtransport.ProxyConfig{
			Address:  request.proxyAddress,
			Username: request.proxyUsername,
			Password: request.proxyPassword,
		}
	}
	var stateClient *tgtransport.Client
	client, err := tgtransport.NewClient(tgtransport.Config{
		AppID:             request.appID,
		APIHash:           request.apiHash,
		BotToken:          request.botToken,
		Proxy:             proxyConfig,
		UploadConcurrency: normalizeUploadConcurrency(request.uploadConcurrency),
		SessionStorage:    credentials.NewSessionStorage(u.secrets, u.paths.Session),
	}, tgtransport.Events{
		OnConnectionState: func(state tgtransport.ConnectionState) {
			u.handleConnectionState(stateClient, generation, state)
		},
		OnFloodWait: func(wait time.Duration) {
			if stateClient == nil || !u.isUsableCurrentClient(stateClient, generation) {
				return
			}
			u.setConnectionStatus("Telegram 要求等待 " + formatDuration(wait))
		},
	})
	if err != nil {
		u.handleConnectionFailure(err, automatic, false, generation)
		return
	}
	stateClient = client
	request.uploadConcurrency = normalizeUploadConcurrency(u.settingsSnapshot().UploadConcurrency)
	client.SetUploadConcurrency(request.uploadConcurrency)

	ctx, cancel := context.WithCancel(u.rootCtx)
	u.clientMu.Lock()
	if generation != u.clientGeneration || u.isClosed() {
		if generation == u.clientGeneration {
			u.connecting = false
		}
		u.clientMu.Unlock()
		cancel()
		return
	}
	u.client = client
	u.clientCancel = cancel
	if u.hasAppliedConnection {
		u.appliedConnection.uploadConcurrency = request.uploadConcurrency
	}
	// Publish the transport while holding the same lifecycle lock used by
	// forceClose. This prevents a closing window from clearing the gateway and
	// then being overwritten by this now-stale connection attempt.
	u.controller.SetGateway(client)
	u.clientMu.Unlock()

	u.doUI(func() { u.connection.SetText("正在启动 Telegram 客户端……") })
	u.startConnectionWorker(func() { u.runClient(client, ctx, automatic, generation) })
	u.startConnectionWorker(func() { u.waitClientReady(client, ctx, automatic, generation) })
	u.startConnectionWorker(func() { u.observeBinding(client, ctx, generation) })
}

func (u *window) selectedUploadConcurrency() int {
	if u.uploadConcurrency != nil {
		if value, ok := uploadConcurrencyForOption(u.uploadConcurrency.Selected); ok {
			return value
		}
	}
	return normalizeUploadConcurrency(u.settingsSnapshot().UploadConcurrency)
}

func (u *window) handleConnectionFailure(err error, automatic, retryable bool, generation uint64) {
	_ = diagnostics.Logf(
		"telegram_connect_failed generation=%d automatic=%t retryable=%t error_type=%T",
		generation,
		automatic,
		retryable,
		err,
	)
	u.clientMu.Lock()
	if generation != u.clientGeneration {
		u.clientMu.Unlock()
		return
	}
	u.connecting = false
	u.clientMu.Unlock()
	if !automatic {
		u.showError(err)
	}
	u.doUI(func() {
		u.clientMu.RLock()
		currentGeneration := generation == u.clientGeneration
		u.clientMu.RUnlock()
		if !currentGeneration {
			return
		}
		u.connected = false
		if !retryable {
			u.resetConnectionRetry()
		}
		if retryable {
			u.connectionConfigErr = nil
		} else {
			u.connectionConfigErr = err
		}
		u.connectButton.SetText("保存并连接")
		u.connectButton.Enable()
		if retryable {
			u.connection.SetText("自动连接失败，将稍后重试")
			u.operationLabel.SetText("Telegram 连接暂时不可用，正在等待自动重试")
		} else {
			u.connection.SetText("连接失败，请检查设置")
			u.operationLabel.SetText(err.Error())
		}
		u.updateActionAvailability()
		if retryable {
			u.scheduleConnectionRetry()
		}
		u.updateScheduleStatus()
		u.updateReadinessHint()
	})
}

func (u *window) handleConnectionState(client *tgtransport.Client, generation uint64, state tgtransport.ConnectionState) {
	_ = diagnostics.Logf("telegram_connection_state generation=%d state=%s", generation, state)
	u.doUI(func() {
		if client == nil || !u.isUsableCurrentClient(client, generation) || u.isClosed() {
			return
		}
		switch state {
		case tgtransport.StateConnecting:
			u.connected = false
			u.connection.SetText("正在连接 Telegram……")
		case tgtransport.StateReady:
			u.connection.SetText("Telegram 连接已建立")
			if u.identity.Username != "" {
				u.connected = true
				u.resetConnectionRetry()
				if u.scheduler != nil {
					u.scheduler.RetryDue()
				}
			}
		case tgtransport.StateDisconnected:
			u.connected = false
			u.connection.SetText("Telegram 连接已断开")
		}
		u.updateActionAvailability()
		u.updateScheduleStatus()
		u.updateReadinessHint()
	})
}

func (u *window) runClient(client *tgtransport.Client, ctx context.Context, automatic bool, generation uint64) {
	err := client.Run(ctx)
	if errors.Is(err, tgtransport.ErrWrongBotSession) {
		if removeErr := os.Remove(u.paths.Session); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("清理旧 Bot 会话失败：%w", removeErr))
		} else {
			err = fmt.Errorf("%w；旧的加密 Bot 会话已清理，请重新连接", err)
		}
	}
	// A failed Run must also stop the readiness and binding observers.  A
	// normal close already cancelled this context, so this is harmless there.
	u.clientMu.Lock()
	wasCurrent := u.client == client && u.clientGeneration == generation
	reconnect := false
	var reconnectRequest *connectionRequest
	if wasCurrent {
		if u.clientCancel != nil {
			u.clientCancel()
		}
		u.clientCancel = nil
		u.client = nil
		// Keep other automatic connection paths out until the UI-side cleanup
		// below either starts the requested replacement or schedules retry.
		u.connecting = true
		reconnect = u.reconnectRequested
		if reconnect && u.pendingReconnect != nil {
			pending := *u.pendingReconnect
			reconnectRequest = &pending
		}
		u.reconnectRequested = false
		u.pendingReconnect = nil
	}
	u.clientMu.Unlock()
	if wasCurrent {
		// Clear the transport immediately. UI delivery may be coalesced or
		// delayed, while the controller's retry loop must never reuse a finished
		// one-shot Client.
		u.controller.SetGateway(nil)
	}
	retryable := shouldRetryConnection(err, ctx.Err(), u.isClosed())
	_ = diagnostics.Logf(
		"telegram_client_stopped generation=%d automatic=%t retryable=%t context_cancelled=%t error_type=%T",
		generation,
		automatic,
		retryable,
		ctx.Err() != nil,
		err,
	)
	if err != nil && !errors.Is(err, context.Canceled) && !u.isClosed() && !automatic && !retryable {
		u.showError(err)
	}
	u.doUI(func() {
		if !wasCurrent {
			return
		}
		u.clientMu.Lock()
		if u.clientGeneration != generation || u.client != nil {
			u.clientMu.Unlock()
			return
		}
		u.connecting = false
		u.clientMu.Unlock()
		u.connected = false
		if !retryable {
			u.resetConnectionRetry()
		}
		u.identity = tgtransport.Identity{}
		if retryable {
			u.connectionConfigErr = nil
		} else if err != nil && !errors.Is(err, context.Canceled) {
			u.connectionConfigErr = err
		}
		u.connectButton.SetText("保存并连接")
		u.connectButton.Enable()
		u.bindButton.Disable()
		u.limit.SetText("当前上传上限：未获取")
		if retryable {
			u.connection.SetText("自动连接失败，将稍后重试")
			u.operationLabel.SetText("Telegram 连接暂时不可用，正在等待自动重试")
		} else if err != nil && !errors.Is(err, context.Canceled) {
			u.connection.SetText("连接失败，请检查设置")
			u.operationLabel.SetText(err.Error())
		} else {
			u.connection.SetText("未连接")
		}
		u.updateActionAvailability()
		if retryable {
			u.scheduleConnectionRetry()
		}
		u.updateScheduleStatus()
		u.updateReadinessHint()
		if reconnect && reconnectRequest != nil && !u.isClosed() {
			u.connectWithRequest(*reconnectRequest, false)
		}
	})
}

func (u *window) waitClientReady(client *tgtransport.Client, ctx context.Context, automatic bool, generation uint64) {
	identity, err := client.WaitReady(ctx)
	if err != nil {
		// runClient owns cleanup and retry scheduling. Waiting here prevents a
		// failed readiness observer from racing a still-running one-shot Client.
		return
	}
	if !u.isUsableCurrentClient(client, generation) || u.isClosed() {
		return
	}
	_ = diagnostics.Logf("telegram_client_ready generation=%d max_upload_bytes=%d", generation, identity.MaxUploadBytes)
	u.doUI(func() {
		u.clientMu.Lock()
		if client == nil || u.client != client || u.clientGeneration != generation || u.reconnectRequested || u.isClosed() {
			u.clientMu.Unlock()
			return
		}
		u.connecting = false
		u.clientMu.Unlock()
		u.connected = true
		u.connectionConfigErr = nil
		u.identity = identity
		u.resetConnectionRetry()
		u.connectButton.SetText("保存并重新连接")
		if u.snapshot.Running {
			u.connectButton.Disable()
		} else {
			u.connectButton.Enable()
		}
		u.bindButton.Enable()
		u.connection.SetText("已连接 Bot：@" + identity.Username)
		limitPrefix := "当前上传上限："
		if !identity.MaxUploadExact {
			limitPrefix = "协议上限（服务端动态值未确认）："
		}
		u.limit.SetText(limitPrefix + formatBytes(identity.MaxUploadBytes))
		u.updateActionAvailability()
		u.dispatchControllerSnapshot(u.controller.Snapshot())
		if u.scheduler != nil {
			u.scheduler.RetryDue()
		}
	})

	// Queue entries may have been added while Telegram was disconnected. Apply
	// the server limit in place; never rescan LastFolder because the durable
	// queue can now contain selections from several different folders.
	if !u.isUsableCurrentClient(client, generation) || u.isClosed() {
		return
	}
	queueSnapshot := u.controller.Snapshot()
	if len(queueSnapshot.Jobs) > 0 {
		if err := u.controller.ApplyUploadLimit(identity.MaxUploadBytes); err != nil {
			if u.isUsableCurrentClient(client, generation) {
				u.showError(err)
			}
		}
	}
}

func (u *window) observeBinding(client *tgtransport.Client, ctx context.Context, generation uint64) {
	for {
		select {
		case event, ok := <-client.BindingEvents():
			if !ok {
				return
			}
			if event.Err != nil {
				if u.isUsableCurrentClient(client, generation) {
					u.showError(event.Err)
				}
				continue
			}
			channel := event.Channel
			u.clientMu.RLock()
			if u.client != client || u.clientGeneration != generation || u.reconnectRequested || u.isClosed() {
				u.clientMu.RUnlock()
				continue
			}
			err := u.controller.SetChannel(channel)
			u.clientMu.RUnlock()
			if err != nil {
				u.showError(err)
				continue
			}
			u.doUI(func() {
				if !u.isUsableCurrentClient(client, generation) {
					return
				}
				u.bindMu.Lock()
				d := u.bindDialog
				u.bindDialog = nil
				u.bindMu.Unlock()
				if d != nil {
					d.Dismiss()
				}
				title := channel.Title
				if title == "" {
					title = fmt.Sprintf("频道 %d", channel.ID)
				}
				u.channel.SetText("已绑定：" + title)
				u.operationLabel.SetText("频道绑定成功")
				u.updateReadinessHint()
				u.updateActionAvailability()
				u.updateScheduleStatus()
				if u.scheduler != nil {
					u.scheduler.RetryDue()
				}
			})
		case <-ctx.Done():
			return
		}
	}
}

func (u *window) beginBinding() {
	client := u.currentClient()
	if client == nil || !u.connected {
		u.showError(errors.New("请先连接 Telegram Bot"))
		return
	}
	code, err := client.BeginChannelBinding()
	if err != nil {
		u.showError(err)
		return
	}

	instruction := widget.NewLabel("请把 Bot 设为目标频道管理员，并在目标频道发送下面这条临时消息。验证成功后应用会自动保存频道信息。")
	codeEntry := widget.NewEntry()
	codeEntry.SetText(code)
	copyButton := widget.NewButton("复制验证码", func() {
		u.application.Clipboard().SetContent(code)
		u.operationLabel.SetText("验证码已复制")
	})
	content := container.NewVBox(instruction, codeEntry)
	d := dialog.NewCustom("绑定频道", "关闭", content, u.window)
	d.SetOnClosed(func() {
		u.bindMu.Lock()
		if u.bindDialog == d {
			u.bindDialog = nil
		}
		u.bindMu.Unlock()
	})
	d.SetButtons([]fyne.CanvasObject{copyButton, widget.NewButton("关闭", func() { d.Dismiss() })})
	u.bindMu.Lock()
	u.bindDialog = d
	u.bindMu.Unlock()
	d.Show()
}

func (u *window) ensureScheduledConnection() {
	u.ensureConnection()
}

func (u *window) ensureConnection() {
	if u.currentClient() != nil || u.isClosed() || u.connectionConfigErr != nil {
		return
	}
	u.clientMu.RLock()
	connecting := u.connecting
	u.clientMu.RUnlock()
	if connecting {
		return
	}
	u.connected = false
	if !u.canAttemptAutomaticConnection() {
		u.updateScheduleStatus()
		u.updateReadinessHint()
		return
	}
	u.connectWithMode(true)
}

func (u *window) scheduleConnectionRetry() {
	if u.isClosed() || u.connected || !u.canAttemptAutomaticConnection() {
		return
	}
	u.connectionRetryMu.Lock()
	if u.connectionRetryTimer != nil {
		u.connectionRetryMu.Unlock()
		return
	}
	u.connectionRetryDelay = nextConnectionRetryDelay(u.connectionRetryDelay)
	delay := u.connectionRetryDelay
	u.connectionRetryTimer = time.AfterFunc(delay, func() {
		u.connectionRetryMu.Lock()
		u.connectionRetryTimer = nil
		u.connectionRetryMu.Unlock()
		u.doUI(u.ensureConnection)
	})
	u.connectionRetryMu.Unlock()
	u.operationLabel.SetText(fmt.Sprintf("Telegram 连接将在 %s 后自动重试", formatDuration(delay)))
	u.updateScheduleStatus()
}

func (u *window) resetConnectionRetry() {
	u.connectionRetryDelay = 0
	u.stopConnectionRetryTimer()
}

func (u *window) stopConnectionRetryTimer() {
	u.connectionRetryMu.Lock()
	if u.connectionRetryTimer != nil {
		u.connectionRetryTimer.Stop()
		u.connectionRetryTimer = nil
	}
	u.connectionRetryMu.Unlock()
}

func nextConnectionRetryDelay(previous time.Duration) time.Duration {
	if previous < connectionRetryInitial {
		return connectionRetryInitial
	}
	next := previous * 2
	if next > connectionRetryMaximum {
		return connectionRetryMaximum
	}
	return next
}

func (u *window) currentClient() *tgtransport.Client {
	u.clientMu.RLock()
	defer u.clientMu.RUnlock()
	return u.client
}

func (u *window) isConnecting() bool {
	u.clientMu.RLock()
	defer u.clientMu.RUnlock()
	return u.connecting
}

func (u *window) isUsableCurrentClient(client *tgtransport.Client, generation uint64) bool {
	u.clientMu.RLock()
	defer u.clientMu.RUnlock()
	return client != nil && u.client == client && u.clientGeneration == generation && !u.reconnectRequested
}

func isTransientConnectionError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, tgtransport.ErrWrongBotSession) {
		return false
	}
	if errors.Is(err, tgtransport.ErrNotConnected) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var networkErr net.Error
	return errors.As(err, &networkErr)
}

func shouldRetryConnection(runErr, contextErr error, closed bool) bool {
	if closed {
		return false
	}
	if runErr == nil {
		return contextErr == nil
	}
	return isTransientConnectionError(runErr)
}
