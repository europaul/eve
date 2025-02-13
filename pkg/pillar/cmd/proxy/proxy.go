// Copyright (c) 2025 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"github.com/lf-edge/eve/pkg/pillar/agentlog"
	"github.com/lf-edge/eve/pkg/pillar/cmd/zedagent"
	"github.com/sirupsen/logrus"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/lf-edge/eve/pkg/pillar/agentbase"
	"github.com/lf-edge/eve/pkg/pillar/base"
	"github.com/lf-edge/eve/pkg/pillar/pubsub"
	"github.com/lf-edge/eve/pkg/pillar/types"
	"github.com/lf-edge/eve/pkg/pillar/utils/wait"
	"github.com/lf-edge/eve/pkg/pillar/zedcloud"
)

const (
	agentName       = "proxy"
	warningTime     = 40 * time.Second
	errorTime       = 3 * time.Minute
	localListenAddr = "127.0.0.1:8999" // Where vmagent will connect
)

var log *base.LogObject

type proxyContext struct {
	agentbase.AgentBase

	// Subscriptions
	subDeviceNetworkStatus pubsub.Subscription
	subGlobalConfig        pubsub.Subscription

	// Current device network info
	deviceNetworkStatus types.DeviceNetworkStatus

	// Provide a zedcloud context for sending requests
	zedcloudCtx zedcloud.ZedCloudContext

	// TLS config to communicate with the controller
	tlsConfig *tls.Config

	// For demonstration, we store the server name here
	serverName   string
	globalConfig *types.ConfigItemValueMap

	// GCInitialized is set to true when we have received the initial GlobalConfig
	GCInitialized bool

	Logger *logrus.Logger
	Log    *base.LogObject
	PubSub *pubsub.PubSub

	// Certificates
	cert             *tls.Certificate
	usingOnboardCert bool
}

// Run is the main entry for this proxy agent
func Run(ps *pubsub.PubSub, loggerArg *logrus.Logger, logArg *base.LogObject, arguments []string, baseDir string) int { //nolint:gocyclo
	ctx := &proxyContext{
		Logger: loggerArg,
		Log:    logArg,
		PubSub: ps,
	}

	log = logArg

	// Initialize the base agent
	agentbase.Init(ctx, loggerArg, log, agentName,
		agentbase.WithBaseDir(baseDir),
		agentbase.WithPidFile(),
		agentbase.WithArguments(arguments))

	log.Noticef("#ohm: Starting %s", agentName)

	// Subscribe to DeviceNetworkStatus
	subDNS, err := ps.NewSubscription(pubsub.SubscriptionOptions{
		AgentName:     "nim",
		MyAgentName:   agentName,
		TopicImpl:     types.DeviceNetworkStatus{},
		CreateHandler: handleDeviceNetworkStatusCreate,
		ModifyHandler: handleDeviceNetworkStatusModify,
		DeleteHandler: handleDeviceNetworkStatusDelete,
		WarningTime:   warningTime,
		ErrorTime:     errorTime,
		Activate:      true,
		Ctx:           ctx,
	})
	if err != nil {
		log.Fatal(err)
	}
	ctx.subDeviceNetworkStatus = subDNS
	subDNS.Activate()

	// Subscribe to GlobalConfig
	subGlobalConfig, err := ps.NewSubscription(
		pubsub.SubscriptionOptions{
			AgentName:     "zedagent",
			MyAgentName:   agentName,
			TopicImpl:     types.ConfigItemValueMap{},
			Persistent:    true,
			Activate:      false,
			Ctx:           ctx,
			CreateHandler: handleGlobalConfigCreate,
			ModifyHandler: handleGlobalConfigModify,
			DeleteHandler: handleGlobalConfigDelete,
			WarningTime:   warningTime,
			ErrorTime:     errorTime,
		})
	if err != nil {
		log.Fatal(err)
	}
	ctx.subGlobalConfig = subGlobalConfig
	subGlobalConfig.Activate()

	// Wait until onboarding is done
	err = wait.WaitForOnboarded(ps, log, agentName, warningTime, errorTime)
	if err != nil {
		log.Fatal("Failed to wait for onboarded")
	}

	// Wait for initial GlobalConfig
	for !ctx.GCInitialized {
		log.Noticef("Waiting for GCInitialized")
		select {
		case change := <-subGlobalConfig.MsgChan():
			subGlobalConfig.ProcessChange(change)
		}
	}
	log.Noticef("processed GlobalConfig")

	// Read the server address (Controller) from /config/server, retry until it is available
	// Retry not more than errorTime
	start := time.Now()
	server, err := os.ReadFile(types.ServerFileName)
	for err != nil {
		if time.Since(start) > errorTime {
			log.Fatalf("#ohm: Failed to read server file: %v", err)
		}
		log.Errorf("#ohm: Failed to read server file: %v, retry", err)
		time.Sleep(5 * time.Second)
		server, err = os.ReadFile(types.ServerFileName)
	}
	serverNameAndPort := strings.TrimSpace(string(server))
	ctx.serverName = strings.Split(serverNameAndPort, ":")[0]

	// Initialize a zedcloud context for multi‐interface sending
	zctx := zedcloud.NewContext(log, zedcloud.ContextOptions{
		SendTimeout: ctx.globalConfig.GlobalValueInt(types.NetworkSendTimeout),
		DialTimeout: ctx.globalConfig.GlobalValueInt(types.NetworkDialTimeout),
		AgentName:   agentName,
	})
	ctx.zedcloudCtx = zctx

	log.Noticef("#ohm: going to startLocalServer")

	// Start the local HTTP server in a separate goroutine
	go startLocalServer(ctx)

	// Periodically show we are alive
	stillRunning := time.NewTicker(25 * time.Second)
	for {
		select {
		case change := <-subDNS.MsgChan():
			subDNS.ProcessChange(change)

		case <-stillRunning.C:
			ps.StillRunning(agentName, warningTime, errorTime)
		}
	}
}

// startLocalServer runs an HTTP server at `localListenAddr` that
// proxies requests to the EVE controller via zedcloud.
func startLocalServer(ctx *proxyContext) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		ctx.handleProxyRequest(w, r)
	})

	srv := &http.Server{
		Addr:    localListenAddr,
		Handler: mux,
	}
	ctx.Logger.Infof("#ohm: Starting proxy on %s", localListenAddr)
	if err := srv.ListenAndServe(); err != nil {
		ctx.Logger.Fatalf("#ohm: ListenAndServe: %v", err)
	}
}

// handleProxyRequest receives an incoming local HTTP request
// (from vmagent) and forwards it to the real controller.
func (ctx *proxyContext) handleProxyRequest(w http.ResponseWriter, r *http.Request) {
	// 1. Read the request body
	log.Noticef("#ohm: Received request %s %s", r.Method, r.URL.Path)
	reqBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to read body: %v", err), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// 2. Construct the target URL on the controller side
	//    For example, you might map local "/foo" => "https://<controller>/api/foo"
	//    For demonstration: just replace the prefix or hard-code a path
	controllerURL := fmt.Sprintf("https://%s%s", ctx.serverName, r.URL.Path)
	log.Noticef("#ohm: Forwarding to %s", controllerURL)

	done, respInfo := securedPost(&ctx.zedcloudCtx, ctx.tlsConfig, controllerURL, false, 0, int64(len(reqBody)), bytes.NewBuffer(reqBody))
	log.Noticef("#ohm: Response from controller: %v, %v", done, respInfo)

	if !done {
		// Some error or non-200 from the controller
		// respInfo may have partial info
		http.Error(w, "Controller request failed", http.StatusBadGateway)
		return
	}
	if respInfo.HTTPResp == nil {
		http.Error(w, "No HTTP response from controller", http.StatusBadGateway)
		return
	}
	defer respInfo.HTTPResp.Body.Close()

	// 4. Forward status code and response headers
	for k, vv := range respInfo.HTTPResp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(respInfo.HTTPResp.StatusCode)

	// 5. Copy response body
	respBody, _ := io.ReadAll(respInfo.HTTPResp.Body)
	w.Write(respBody)
}

func handleDeviceNetworkStatusCreate(contextArg interface{}, _ string, statusArg interface{}) {
	ctx := contextArg.(*proxyContext)
	status := statusArg.(types.DeviceNetworkStatus)
	if cmp.Equal(ctx.deviceNetworkStatus, status) {
		return
	}
	ctx.deviceNetworkStatus = status
}

func handleDeviceNetworkStatusModify(contextArg interface{}, key string, statusArg interface{}, oldStatusArg interface{}) {
	ctx := contextArg.(*proxyContext)
	status := statusArg.(types.DeviceNetworkStatus)
	if cmp.Equal(ctx.deviceNetworkStatus, status) {
		return
	}
	// update proxy certs if configured
	if ctx.zedcloudCtx.V2API {
		zedcloud.UpdateTLSProxyCerts(&ctx.zedcloudCtx)
	}
	ctx.deviceNetworkStatus = status
}

// handleDeviceNetworkStatusDelete resets the deviceNetworkStatus
func handleDeviceNetworkStatusDelete(ctxArg interface{}, _ string, _ interface{}) {
	ctx := ctxArg.(*proxyContext)
	ctx.deviceNetworkStatus = types.DeviceNetworkStatus{}
}
func handleGlobalConfigCreate(ctxArg interface{}, key string,
	statusArg interface{}) {
	handleGlobalConfigImpl(ctxArg, key, statusArg)
}

func handleGlobalConfigModify(ctxArg interface{}, key string,
	statusArg interface{}, oldStatusArg interface{}) {
	handleGlobalConfigImpl(ctxArg, key, statusArg)
}

func handleGlobalConfigImpl(ctxArg interface{}, key string, statusArg interface{}) {
	ctx := ctxArg.(*proxyContext)

	ctx.Logger.Infof("handleGlobalConfigImpl for %s", key)
	gcp := agentlog.HandleGlobalConfig(ctx.Log, ctx.subGlobalConfig, agentName,
		ctx.CLIParams().DebugOverride, ctx.Logger)
	if gcp != nil {
		ctx.globalConfig = gcp
	}
	ctx.GCInitialized = true
	ctx.Logger.Infof("handleGlobalConfigImpl done for %s", key)
}

func handleGlobalConfigDelete(ctxArg interface{}, key string,
	statusArg interface{}) {

	ctx := ctxArg.(*proxyContext)
	ctx.Logger.Infof("handleGlobalConfigDelete for %s", key)
	agentlog.HandleGlobalConfig(ctx.Log, ctx.subGlobalConfig, agentName,
		ctx.CLIParams().DebugOverride, ctx.Logger)
	*ctx.globalConfig = *types.DefaultConfigItemValueMap()
	ctx.Logger.Infof("handleGlobalConfigDelete done for %s", key)
}

func securedPost(_ *zedcloud.ZedCloudContext, tlsConfig *tls.Config,
	requrl string, skipVerify bool, retryCount int,
	reqlen int64, b *bytes.Buffer) (done bool, rv zedcloud.SendRetval) {

	zedcloudCtx := zedagent.GetZedcloudContext()

	ctxWork, cancel := zedcloud.GetContextForAllIntfFunctions(zedcloudCtx)
	defer cancel()

	rv, err := zedcloud.SendOnAllIntf(ctxWork, zedcloudCtx, requrl, reqlen, b, retryCount,
		false, false)
	if err != nil {
		log.Errorf("SendOnAllIntf failed: %s, %v", err, rv.HTTPResp)
		return false, rv
	}

	log.Noticef("#ohm: Received status %s", rv.HTTPResp.Status)
	log.Noticef("#ohm: Received reply %s", string(rv.RespContents))

	if rv.HTTPResp.StatusCode != http.StatusOK && rv.HTTPResp.StatusCode != http.StatusNoContent {
		log.Errorf("%s failed: %s", requrl, rv.HTTPResp.Status)
		return false, rv
	}

	// Print the body if it is not empty
	log.Noticef("#ohm: Received reply %s", string(rv.RespContents))
	// Print headers
	for k, v := range rv.HTTPResp.Header {
		log.Noticef("#ohm: Header %s: %s", k, v)
	}

	contentType := rv.HTTPResp.Header.Get("Content-Type")
	if contentType == "" {
		log.Errorf("%s no content-type", requrl)
		return false, rv
	}
	mimeType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		log.Errorf("%s ParseMediaType failed %v", requrl, err)
		return false, rv
	}
	switch mimeType {
	case "application/x-proto-binary", "application/json", "text/plain":
		log.Tracef("Received reply %s", string(rv.RespContents))
	default:
		log.Errorln("Incorrect Content-Type " + mimeType)
		return false, rv
	}
	if len(rv.RespContents) == 0 {
		return true, rv
	}

	log.Noticef("#ohm: Went to removeAndVerifyAuthContainer!")
	err = zedcloud.RemoveAndVerifyAuthContainer(zedcloudCtx, &rv, skipVerify)
	if err != nil {
		log.Errorf("RemoveAndVerifyAuthContainer failed: %s", err)
		return false, rv
	}
	log.Noticef("#ohm: Done with removeAndVerifyAuthContainer!")
	return true, rv
}
