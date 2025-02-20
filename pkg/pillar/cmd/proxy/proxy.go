// Copyright (c) 2025 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"github.com/lf-edge/eve/pkg/pillar/agentlog"
	"github.com/lf-edge/eve/pkg/pillar/cmd/zedagent"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/lf-edge/eve/pkg/pillar/agentbase"
	"github.com/lf-edge/eve/pkg/pillar/base"
	"github.com/lf-edge/eve/pkg/pillar/cmd/vcomlink"
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
		//ctx.handleProxyRequest(w, r)
		ctx.handleProxyVsockRequest(w, r)
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

func (ctx *proxyContext) handleProxyVsockRequest(w http.ResponseWriter, r *http.Request) {
	log.Noticef("#ohm: Received request %s %s", r.Method, r.URL.Path)

	// Prepare the request to be sent to the controller: headers, body, etc.
	reqBody, err := io.ReadAll(r.Body)
	if err != nil {
		log.Errorf("#ohm: Failed to read body: %v", err)
		http.Error(w, fmt.Sprintf("Failed to read body: %v", err), http.StatusBadRequest)
		return
	}

	var vmCID uint32
	var vmPort uint32 = 2412
	var reqBytes []byte

	// Form the request bytes, starting with the path and method
	firstLine := fmt.Sprintf("%s %s HTTP/1.1\r\n", r.Method, r.URL.Path)
	reqBytes = append(reqBytes, []byte(firstLine)...)

	mandatoryHeaders := []string{"Content-Encoding", "Content-Type", "User-Agent", "X-Prometheus-Remote-Write-Version", "Host"}
	defaultHeaders := map[string]string{
		"Content-Encoding":                  "snappy",
		"Content-Type":                      "application/x-protobuf",
		"User-Agent":                        "pillar",
		"X-Prometheus-Remote-Write-Version": "0.1.0",
		"Host":                              "localhost:9009",
	}

	for key, values := range r.Header {
		for _, value := range values {
			log.Noticef("#ohm: Header %s: %s", key, value)
		}
		// Form a string and append to reqBytes
		reqBytes = append(reqBytes, []byte(fmt.Sprintf("%s: %s\r\n", key, values[0]))...)
	}

	for _, header := range mandatoryHeaders {
		if _, ok := r.Header[header]; !ok {
			log.Errorf("#ohm: Missing header %s", header)
			// Add the header with a default value, form a string and append to reqBytes
			reqBytes = append(reqBytes, []byte(fmt.Sprintf("%s: %s\n", header, defaultHeaders[header]))...)
		}
	}

	// Append the last CRLF
	reqBytes = append(reqBytes, []byte("\r\n")...)

	// Print current request bytes
	log.Noticef("#ohm: Request bytes:\n%s", string(reqBytes))

	// Append the request body to reqBytes
	reqBytes = append(reqBytes, reqBody...)
	var resp *http.Response

	// Try sending to a range of vmCIDs until one succeeds.
	for i := 3; i < 10; i++ {
		vmCID = uint32(i)
		resp, err = vsockSend(vmCID, vmPort, reqBytes)
		if err != nil {
			log.Warnf("#ohm: Failed to send via vsock to vmCID %d: %v", vmCID, err)
			continue
		}
		log.Noticef("#ohm: Received response from vsock %d", vmCID)
		break
	}
	if err != nil {
		log.Errorf("#ohm: Failed to send request via vsock: %v", err)
		http.Error(w, fmt.Sprintf("Failed to send request via vsock: %v", err), http.StatusBadGateway)
		return
	}

	// Write the response back to the client
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	w.Write(respBody)

	log.Noticef("#ohm: Response from vsock %d: %v", vmCID, resp.Status)

	return

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
	defer func() {
		if respInfo.HTTPResp.Body != nil {
			respInfo.HTTPResp.Body.Close()
		}
	}()

	// 4. Forward status code and response headers
	for k, vv := range respInfo.HTTPResp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(respInfo.HTTPResp.StatusCode)

	// 5. Copy response body
	if respInfo.HTTPResp.Body != nil {
		respBody, _ := io.ReadAll(respInfo.HTTPResp.Body)
		w.Write(respBody)
	}
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

func dialVsock(cid uint32, port uint32) (net.Conn, error) {
	// Create a vsock socket.
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to create vsock socket: %w", err)
	}

	// Build the vsock address.
	addr := &unix.SockaddrVM{
		CID:  cid,
		Port: port, // ensure proper type conversion if needed
	}
	// Connect to the VM's vsock listener.
	if err := unix.Connect(fd, addr); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("failed to connect vsock socket: %w", err)
	}

	// Instead of wrapping with net.FileConn (which doesn't support vsock),
	// use your custom VSOCKConn that implements net.Conn.
	conn := &vcomlink.VSOCKConn{
		Fd:   fd,
		Addr: addr,
	}
	return conn, nil
}

func vsockSend(vmCID uint32, vmPort uint32, data []byte) (*http.Response, error) {
	// Dial the vsock connection.
	conn, err := dialVsock(vmCID, vmPort)
	if err != nil {
		return nil, fmt.Errorf("failed to dial vsock: %w", err)
	}
	// Do not defer conn.Close() here since the response's Body will wrap this connection.
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	log.Noticef("#ohm: Sending request to vsock %d", vmCID)

	// Write the entire request data to the vsock connection.
	if _, err := conn.Write(data); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to write data to vsock: %w", err)
	}

	log.Noticef("#ohm: Now I will read the response from vsock %d", vmCID)

	// Read in stream mode until a proper HTTP response is received.
	reader := bufio.NewReader(conn)
	log.Noticef("#ohm: Going to read response from vsock %d", vmCID)
	// Create a dummy request to indicate that this is a response to a POST request.
	req := http.Request{
		Method: "POST",
	}
	resp, err := http.ReadResponse(reader, &req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to read HTTP response: %w", err)
	}
	log.Noticef("#ohm: Received response from vsock %d, status %s", vmCID, resp.Status)

	// Print the headers
	for k, v := range resp.Header {
		log.Noticef("#ohm: Header %s: %s", k, v)
	}

	// Check for acceptable status codes.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		conn.Close()
		return nil, fmt.Errorf("unexpected status code: %d, body: %s", resp.StatusCode, body)
	}

	// Return the response; the caller is responsible for closing resp.Body (which will also close the underlying connection).
	return resp, nil
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

	//contentType := rv.HTTPResp.Header.Get("Content-Type")
	//if contentType == "" {
	//	log.Errorf("%s no content-type", requrl)
	//	return false, rv
	//}
	//mimeType, _, err := mime.ParseMediaType(contentType)
	//if err != nil {
	//	log.Errorf("%s ParseMediaType failed %v", requrl, err)
	//	return false, rv
	//}
	//switch mimeType {
	//case "application/x-proto-binary", "application/json", "text/plain":
	//	log.Tracef("Received reply %s", string(rv.RespContents))
	//default:
	//	log.Errorln("Incorrect Content-Type " + mimeType)
	//	return false, rv
	//}
	//if len(rv.RespContents) == 0 {
	//	return true, rv
	//}
	//
	//log.Noticef("#ohm: Went to removeAndVerifyAuthContainer!")
	//err = zedcloud.RemoveAndVerifyAuthContainer(zedcloudCtx, &rv, skipVerify)
	//if err != nil {
	//	log.Errorf("RemoveAndVerifyAuthContainer failed: %s", err)
	//	return false, rv
	//}
	//log.Noticef("#ohm: Done with removeAndVerifyAuthContainer!")
	return true, rv
}
