package main

import (
	"bytes"
	"compress/zlib"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/gorilla/websocket"
)

const (
	USER_AGENT = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/102.0.0.0 Safari/537.36"
	
	UID_INIT_URL             = "https://api.bilibili.com/x/web-interface/nav"
	WBI_INIT_URL             = UID_INIT_URL
	BUVID_INIT_URL          = "https://www.bilibili.com/"
	ROOM_INIT_URL           = "https://api.live.bilibili.com/room/v1/Room/get_info"
	DANMAKU_SERVER_CONF_URL = "https://api.live.bilibili.com/xlive/web-room/v1/index/getDanmuInfo"
)

// 协议版本
const (
	ProtoVerNormal    = 0
	ProtoVerHeartbeat = 1
	ProtoVerDeflate   = 2
	ProtoVerBrotli    = 3
)

// 操作类型
const (
	OpHandshake      = 0
	OpHandshakeReply = 1
	OpHeartbeat      = 2
	OpHeartbeatReply = 3
	OpSendMsg        = 4
	OpSendMsgReply   = 5
	OpDisconnectReply = 6
	OpAuth           = 7
	OpAuthReply      = 8
	OpRaw            = 9
	OpProtoReady     = 10
	OpProtoFinish    = 11
	OpChangeRoom     = 12
	OpChangeRoomReply = 13
	OpRegister       = 14
	OpRegisterReply  = 15
	OpUnregister     = 16
	OpUnregisterReply = 17
)

// 消息头结构
type Header struct {
	PackLen       uint32
	RawHeaderSize uint16
	Ver           uint16
	Operation     uint32
	SeqId         uint32
}

// WBI签名器
type WbiSigner struct {
	session        *http.Client
	wbiKey         string
	refreshFuture  sync.Mutex
	lastRefreshTime time.Time
}

var wbiKeyIndexTable = []int{
	46, 47, 18, 2, 53, 8, 23, 32, 15, 50, 10, 31, 58, 3, 45, 35,
	27, 43, 5, 49, 33, 9, 42, 19, 29, 28, 14, 39, 12, 38, 41, 13,
}

func NewWbiSigner(client *http.Client) *WbiSigner {
	return &WbiSigner{
		session: client,
	}
}

func (w *WbiSigner) NeedRefreshWbiKey() bool {
	return w.wbiKey == "" || time.Since(w.lastRefreshTime) >= 11*time.Hour+59*time.Minute+30*time.Second
}

func (w *WbiSigner) RefreshWbiKey() error {
	w.refreshFuture.Lock()
	defer w.refreshFuture.Unlock()
	
	wbiKey, err := w.getWbiKey()
	if err != nil {
		return err
	}
	
	w.wbiKey = wbiKey
	w.lastRefreshTime = time.Now()
	return nil
}

func (w *WbiSigner) getWbiKey() (string, error) {
	req, err := http.NewRequest("GET", WBI_INIT_URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", USER_AGENT)
	
	resp, err := w.session.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	
	var data struct {
		Data struct {
			WbiImg struct {
				ImgUrl string `json:"img_url"`
				SubUrl string `json:"sub_url"`
			} `json:"wbi_img"`
		} `json:"data"`
	}
	
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", err
	}
	
	// 提取img_key和sub_key
	imgKey := strings.Split(strings.Split(data.Data.WbiImg.ImgUrl, "/")[len(strings.Split(data.Data.WbiImg.ImgUrl, "/"))-1], ".")[0]
	subKey := strings.Split(strings.Split(data.Data.WbiImg.SubUrl, "/")[len(strings.Split(data.Data.WbiImg.SubUrl, "/"))-1], ".")[0]
	
	shuffledKey := imgKey + subKey
	var wbiKey []byte
	for _, index := range wbiKeyIndexTable {
		if index < len(shuffledKey) {
			wbiKey = append(wbiKey, shuffledKey[index])
		}
	}
	
	return string(wbiKey), nil
}

func (w *WbiSigner) AddWbiSign(params map[string]string) map[string]string {
	if w.wbiKey == "" {
		return params
	}
	
	wts := strconv.FormatInt(time.Now().Unix(), 10)
	paramsToSign := make(map[string]string)
	for k, v := range params {
		paramsToSign[k] = v
	}
	paramsToSign["wts"] = wts
	
	// 排序键
	keys := make([]string, 0, len(paramsToSign))
	for k := range paramsToSign {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	
	// 构建签名字符串
	var values []string
	for _, k := range keys {
		v := paramsToSign[k]
		// 过滤特殊字符
		var filtered strings.Builder
		for _, ch := range v {
			if ch != '!' && ch != '\'' && ch != '(' && ch != ')' && ch != '*' {
				filtered.WriteRune(ch)
			}
		}
		values = append(values, k+"="+filtered.String())
	}
	
	strToSign := strings.Join(values, "&") + w.wbiKey
	hash := md5.Sum([]byte(strToSign))
	wRid := hex.EncodeToString(hash[:])
	
	result := make(map[string]string)
	for k, v := range params {
		result[k] = v
	}
	result["wts"] = wts
	result["w_rid"] = wRid
	
	return result
}

// B站客户端
type BiliBiliClient struct {
	roomID           int
	uid              int
	session          *http.Client
	wbiSigner        *WbiSigner
	heartbeatInterval time.Duration
	
	realRoomID       int
	roomOwnerUID     int
	hostServerList   []map[string]interface{}
	hostServerToken  string
	
	conn             *websocket.Conn
	connMutex        sync.Mutex
	running          bool
	runningMutex     sync.Mutex
	
	buvid            string
}

func NewBiliBiliClient(roomID int) *BiliBiliClient {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Timeout: 10 * time.Second,
		Jar:     jar,
	}
	
	return &BiliBiliClient{
		roomID:           roomID,
		session:          client,
		wbiSigner:        NewWbiSigner(client),
		heartbeatInterval: 30 * time.Second,
	}
}

func (c *BiliBiliClient) Start() {
	c.runningMutex.Lock()
	defer c.runningMutex.Unlock()
	
	if c.running {
		return
	}
	
	c.running = true
	go c.networkCoroutine()
}

func (c *BiliBiliClient) Stop() {
	c.runningMutex.Lock()
	defer c.runningMutex.Unlock()
	
	c.running = false
	
	c.connMutex.Lock()
	if c.conn != nil {
		c.conn.Close()
	}
	c.connMutex.Unlock()
}

func (c *BiliBiliClient) networkCoroutine() {
	retryCount := 0
	
	for {
		c.runningMutex.Lock()
		if !c.running {
			c.runningMutex.Unlock()
			break
		}
		c.runningMutex.Unlock()
		
		// 初始化房间信息
		if err := c.initRoom(); err != nil {
			log.Printf("初始化房间失败: %v", err)
			time.Sleep(time.Second)
			continue
		}
		
		// 连接WebSocket
		wsURL := c.getWsURL(retryCount)
		dialer := websocket.Dialer{}
		header := http.Header{}
		header.Set("User-Agent", USER_AGENT)
		
		conn, _, err := dialer.Dial(wsURL, header)
		if err != nil {
			log.Printf("WebSocket连接失败: %v", err)
			retryCount++
			time.Sleep(time.Second)
			continue
		}
		
		c.connMutex.Lock()
		c.conn = conn
		c.connMutex.Unlock()
		
		// 发送认证
		if err := c.sendAuth(); err != nil {
			log.Printf("发送认证失败: %v", err)
			conn.Close()
			continue
		}
		
		// 启动心跳
		go c.heartbeatLoop()
		
		// 读取消息
		c.readLoop()
		
		retryCount++
		time.Sleep(time.Second)
	}
}

func (c *BiliBiliClient) initRoom() error {
	// 初始化UID
	if err := c.initUID(); err != nil {
		return err
	}
	
	// 初始化BUVID
	if err := c.initBuvid(); err != nil {
		return err
	}
	
	// 初始化房间ID和房主
	if err := c.initRoomIDAndOwner(); err != nil {
		return err
	}
	
	// 初始化服务器信息
	if err := c.initHostServer(); err != nil {
		return err
	}
	
	return nil
}

func (c *BiliBiliClient) initUID() error {
	// 从cookie jar获取SESSDATA
	u, _ := url.Parse(UID_INIT_URL)
	cookies := c.session.Jar.Cookies(u)
	hasSessdata := false
	for _, cookie := range cookies {
		if cookie.Name == "SESSDATA" && cookie.Value != "" {
			hasSessdata = true
			break
		}
	}
	
	if !hasSessdata {
		c.uid = 0
		return nil
	}
	
	req, err := http.NewRequest("GET", UID_INIT_URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", USER_AGENT)
	
	resp, err := c.session.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	
	var data struct {
		Data struct {
			IsLogin bool `json:"isLogin"`
			Mid     int  `json:"mid"`
		} `json:"data"`
	}
	
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return err
	}
	
	if !data.Data.IsLogin {
		c.uid = 0
	} else {
		c.uid = data.Data.Mid
	}
	
	return nil
}

func (c *BiliBiliClient) initBuvid() error {
	// 创建一个cookie jar来存储cookies
	jar, _ := cookiejar.New(nil)
	c.session.Jar = jar
	
	req, err := http.NewRequest("GET", BUVID_INIT_URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", USER_AGENT)
	
	resp, err := c.session.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	
	// 读取响应body
	io.ReadAll(resp.Body)
	
	// 从cookie jar中获取buvid3
	u, _ := url.Parse(BUVID_INIT_URL)
	cookies := c.session.Jar.Cookies(u)
	for _, cookie := range cookies {
		if cookie.Name == "buvid3" {
			c.buvid = cookie.Value
			fmt.Printf("获取到的buvid值: %s\n", c.buvid)
			break
		}
	}
	
	return nil
}

func (c *BiliBiliClient) initRoomIDAndOwner() error {
	params := url.Values{}
	params.Set("room_id", strconv.Itoa(c.roomID))
	
	req, err := http.NewRequest("GET", ROOM_INIT_URL+"?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", USER_AGENT)
	
	resp, err := c.session.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	
	var data struct {
		Data struct {
			RoomID int `json:"room_id"`
			UID    int `json:"uid"`
		} `json:"data"`
	}
	
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return err
	}
	
	c.realRoomID = data.Data.RoomID
	c.roomOwnerUID = data.Data.UID
	
	return nil
}

func (c *BiliBiliClient) initHostServer() error {
	if c.wbiSigner.NeedRefreshWbiKey() {
		if err := c.wbiSigner.RefreshWbiKey(); err != nil {
			return err
		}
	}
	
	params := map[string]string{
		"id":   strconv.Itoa(c.realRoomID),
		"type": "0",
	}
	params = c.wbiSigner.AddWbiSign(params)
	
	urlParams := url.Values{}
	for k, v := range params {
		urlParams.Set(k, v)
	}
	
	req, err := http.NewRequest("GET", DANMAKU_SERVER_CONF_URL+"?"+urlParams.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", USER_AGENT)
	
	resp, err := c.session.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	
	var data struct {
		Code int `json:"code"`
		Data struct {
			HostList []map[string]interface{} `json:"host_list"`
			Token    string                   `json:"token"`
		} `json:"data"`
	}
	
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return err
	}
	
	c.hostServerList = data.Data.HostList
	c.hostServerToken = data.Data.Token
	
	return nil
}

func (c *BiliBiliClient) getWsURL(retryCount int) string {
	if len(c.hostServerList) == 0 {
		// 使用默认服务器列表
		return "wss://broadcastlv.chat.bilibili.com:443/sub"
	}
	
	hostServer := c.hostServerList[retryCount%len(c.hostServerList)]
	host, ok := hostServer["host"].(string)
	if !ok {
		return "wss://broadcastlv.chat.bilibili.com:443/sub"
	}
	
	wssPort := 443
	if port, ok := hostServer["wss_port"].(float64); ok {
		wssPort = int(port)
	}
	
	return fmt.Sprintf("wss://%s:%d/sub", host, wssPort)
}

func (c *BiliBiliClient) makePacket(data interface{}, operation uint32) ([]byte, error) {
	var body []byte
	
	switch v := data.(type) {
	case map[string]interface{}:
		var err error
		body, err = json.Marshal(v)
		if err != nil {
			return nil, err
		}
	case string:
		body = []byte(v)
	case []byte:
		body = v
	default:
		return nil, fmt.Errorf("unsupported data type")
	}
	
	header := Header{
		PackLen:       uint32(16 + len(body)),
		RawHeaderSize: 16,
		Ver:           1,
		Operation:     operation,
		SeqId:         1,
	}
	
	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.BigEndian, header); err != nil {
		return nil, err
	}
	buf.Write(body)
	
	return buf.Bytes(), nil
}

func (c *BiliBiliClient) sendAuth() error {
	authParams := map[string]interface{}{
		"uid":      c.uid,
		"roomid":   c.realRoomID,
		"protover": 3,
		"platform": "web",
		"type":     2,
		"buvid":    c.buvid,
	}
	
	fmt.Printf("发送认证时的buvid值: %s\n", c.buvid)
	
	if c.hostServerToken != "" {
		authParams["key"] = c.hostServerToken
	}
	
	packet, err := c.makePacket(authParams, OpAuth)
	if err != nil {
		return err
	}
	
	c.connMutex.Lock()
	defer c.connMutex.Unlock()
	
	if c.conn == nil {
		return fmt.Errorf("connection is nil")
	}
	
	return c.conn.WriteMessage(websocket.BinaryMessage, packet)
}

func (c *BiliBiliClient) heartbeatLoop() {
	ticker := time.NewTicker(c.heartbeatInterval)
	defer ticker.Stop()
	
	for {
		select {
		case <-ticker.C:
			c.runningMutex.Lock()
			if !c.running {
				c.runningMutex.Unlock()
				return
			}
			c.runningMutex.Unlock()
			
			packet, err := c.makePacket(map[string]interface{}{}, OpHeartbeat)
			if err != nil {
				continue
			}
			
			c.connMutex.Lock()
			if c.conn != nil {
				c.conn.WriteMessage(websocket.BinaryMessage, packet)
			}
			c.connMutex.Unlock()
		}
	}
}

func (c *BiliBiliClient) readLoop() {
	c.connMutex.Lock()
	conn := c.conn
	c.connMutex.Unlock()
	
	if conn == nil {
		return
	}
	
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		
		c.parseWsMessage(data)
	}
}

func (c *BiliBiliClient) parseWsMessage(data []byte) {
	offset := 0
	
	for offset < len(data) {
		if len(data[offset:]) < 16 {
			break
		}
		
		var header Header
		buf := bytes.NewReader(data[offset : offset+16])
		if err := binary.Read(buf, binary.BigEndian, &header); err != nil {
			break
		}
		
		if header.Operation == OpSendMsgReply || header.Operation == OpAuthReply {
			body := data[offset+int(header.RawHeaderSize) : offset+int(header.PackLen)]
			c.parseBusinessMessage(header, body)
		}
		
		offset += int(header.PackLen)
	}
}

func (c *BiliBiliClient) parseBusinessMessage(header Header, body []byte) {
	if header.Operation == OpSendMsgReply {
		switch header.Ver {
		case ProtoVerBrotli:
			reader := brotli.NewReader(bytes.NewReader(body))
			decompressed, err := io.ReadAll(reader)
			if err == nil {
				c.parseWsMessage(decompressed)
			}
		case ProtoVerDeflate:
			reader, err := zlib.NewReader(bytes.NewReader(body))
			if err == nil {
				decompressed, err := io.ReadAll(reader)
				if err == nil {
					c.parseWsMessage(decompressed)
				}
				reader.Close()
			}
		case ProtoVerNormal:
			if len(body) > 0 {
				var command map[string]interface{}
				if err := json.Unmarshal(body, &command); err == nil {
					c.handleCommand(command)
				}
			}
		}
	} else if header.Operation == OpAuthReply {
		// 认证回复后发送心跳
		packet, _ := c.makePacket(map[string]interface{}{}, OpHeartbeat)
		c.connMutex.Lock()
		if c.conn != nil {
			c.conn.WriteMessage(websocket.BinaryMessage, packet)
		}
		c.connMutex.Unlock()
	}
}

func (c *BiliBiliClient) handleCommand(command map[string]interface{}) {
	// 打印收到的消息
	jsonData, _ := json.MarshalIndent(command, "", "  ")
	fmt.Printf("收到消息: %s\n", string(jsonData))
	
	cmd, ok := command["cmd"].(string)
	if !ok {
		return
	}
	
	// 处理命令前缀
	if pos := strings.Index(cmd, ":"); pos != -1 {
		cmd = cmd[:pos]
	}
	
	// 处理礼物消息
	if cmd == "SEND_GIFT" {
		if data, ok := command["data"].(map[string]interface{}); ok {
			onGift(data)
		}
	}
}

// 处理礼物消息
func onGift(giftData map[string]interface{}) {
	face := ""
	giftName := ""
	num := 0
	
	if v, ok := giftData["face"].(string); ok {
		face = v
	}
	if v, ok := giftData["giftName"].(string); ok {
		giftName = v
	}
	if v, ok := giftData["num"].(float64); ok {
		num = int(v)
	}
	
	fmt.Printf(" %s 赠送%sx%d\n", face, giftName, num)
}

func main() {
	// 请输入正确的直播间id
	roomID := 1111111111
	
	// 创建客户端
	client := NewBiliBiliClient(roomID)
	
	// 启动客户端
	client.Start()
	
	// 阻塞主线程
	select {}
}