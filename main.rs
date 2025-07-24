use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

use byteorder::{BigEndian, ByteOrder};
use chrono::Utc;
use futures_util::{SinkExt, StreamExt};
use log::{error, info, warn};
use md5;
use reqwest::Client;
use serde::Deserialize;
use serde_json::json;
use tokio::sync::RwLock;
use tokio_tungstenite::tungstenite::protocol::Message;
use flate2::read::ZlibDecoder;
use std::io::Read;

// 常量
const USER_AGENT: &str = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/102.0.0.0 Safari/537.36";
const WBI_INIT_URL: &str = "https://api.bilibili.com/x/web-interface/nav";
const ROOM_INIT_URL: &str = "https://api.live.bilibili.com/room/v1/Room/get_info";
const DANMAKU_SERVER_CONF_URL: &str = "https://api.live.bilibili.com/xlive/web-room/v1/index/getDanmuInfo";

// 操作类型
#[repr(u32)]
enum Operation {
    Heartbeat = 2,
    HeartbeatReply = 3,
    SendMsgReply = 5,
    Auth = 7,
    AuthReply = 8,
}

// 协议版本
#[repr(u16)]
enum ProtoVer {
    Normal = 0,
    Heartbeat = 1,
    Deflate = 2,
    Brotli = 3,
}

// API响应结构
#[derive(Deserialize)]
struct RoomInfoResponse {
    data: RoomInfoData,
}

#[derive(Deserialize)]
struct RoomInfoData {
    room_id: u32,
}

#[derive(Deserialize)]
struct DanmuInfoResponse {
    data: DanmuInfoData,
}

#[derive(Deserialize)]
struct DanmuInfoData {
    token: String,
    host_list: Vec<DanmuServerInfo>,
}

#[derive(Deserialize)]
struct DanmuServerInfo {
    host: String,
    wss_port: u16,
}

// WBI签名器
struct WbiSigner {
    client: Client,
    wbi_key: RwLock<String>,
}

impl WbiSigner {
    const WBI_KEY_INDEX_TABLE: &'static [usize] = &[
        46, 47, 18, 2, 53, 8, 23, 32, 15, 50, 10, 31, 58, 3, 45, 35, 27, 43, 5, 49, 33,
        9, 42, 19, 29, 28, 14, 39, 12, 38, 41, 13,
    ];

    fn new(client: Client) -> Self {
        Self {
            client,
            wbi_key: RwLock::new(String::new()),
        }
    }

    async fn refresh_wbi_key(&self) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
        let res = self.client.get(WBI_INIT_URL).send().await?.json::<serde_json::Value>().await?;
        let wbi_img = &res["data"]["wbi_img"];
        let img_url = wbi_img["img_url"].as_str().unwrap_or_default();
        let sub_url = wbi_img["sub_url"].as_str().unwrap_or_default();

        let img_key = img_url.split('/').last().unwrap_or_default().split('.').next().unwrap_or_default();
        let sub_key = sub_url.split('/').last().unwrap_or_default().split('.').next().unwrap_or_default();
        
        let shuffled_key = format!("{}{}", img_key, sub_key);
        let wbi_key: String = Self::WBI_KEY_INDEX_TABLE
            .iter()
            .filter_map(|&index| shuffled_key.chars().nth(index))
            .collect();
        
        *self.wbi_key.write().await = wbi_key;
        Ok(())
    }

    async fn add_wbi_sign(&self, params: &mut HashMap<String, String>) {
        let wbi_key = self.wbi_key.read().await;
        if wbi_key.is_empty() { return; }

        let wts = Utc::now().timestamp().to_string();
        params.insert("wts".to_string(), wts.clone());

        let mut sorted_params: Vec<_> = params.iter().collect();
        sorted_params.sort_by_key(|(k, _)| *k);

        let query_to_sign: String = sorted_params
            .into_iter()
            .map(|(k, v)| {
                let cleaned_v: String = v.chars().filter(|&c| !"!'()*".contains(c)).collect();
                format!("{}={}", k, cleaned_v)
            })
            .collect::<Vec<_>>()
            .join("&");
        
        let str_to_sign = format!("{}{}", query_to_sign, *wbi_key);
        let digest = md5::compute(str_to_sign.as_bytes());
        let w_rid = format!("{:x}", digest);

        params.insert("w_rid".to_string(), w_rid);
    }
}

// 主客户端
struct BiliBiliClient {
    room_id: u32,
    client: Client,
    wbi_signer: WbiSigner,
    real_room_id: RwLock<u32>,
    token: RwLock<String>,
    ws_url: RwLock<String>,
    buvid: RwLock<String>,
}

impl BiliBiliClient {
    fn new(room_id: u32) -> Self {
        let client = Client::builder()
            .user_agent(USER_AGENT)
            .cookie_store(true)
            .timeout(Duration::from_secs(10))
            .build()
            .unwrap();

        Self {
            room_id,
            wbi_signer: WbiSigner::new(client.clone()),
            client,
            real_room_id: RwLock::new(0),
            token: RwLock::new(String::new()),
            ws_url: RwLock::new(String::new()),
            buvid: RwLock::new(String::new()),
        }
    }

    async fn init(&self) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
        // 初始化cookie
        let response = self.client.get("https://www.bilibili.com/").send().await?;
        // 从response headers中提取buvid
        if let Some(set_cookie) = response.headers().get("set-cookie") {
            if let Ok(cookie_str) = set_cookie.to_str() {
                if let Some(buvid_start) = cookie_str.find("buvid3=") {
                    let buvid_part = &cookie_str[buvid_start + 7..];
                    if let Some(buvid_end) = buvid_part.find(';') {
                        let buvid_value = &buvid_part[..buvid_end];
                        *self.buvid.write().await = buvid_value.to_string();
                        println!("获取到的buvid值: {}", buvid_value);
                    }
                }
            }
        }

        // 获取真实房间号
        let room_info: RoomInfoResponse = self.client
            .get(format!("{}?id={}", ROOM_INIT_URL, self.room_id))
            .send().await?
            .json().await?;
        *self.real_room_id.write().await = room_info.data.room_id;
        info!("真实房间号: {}", room_info.data.room_id);

        // 刷新WBI密钥
        self.wbi_signer.refresh_wbi_key().await?;
        
        // 获取弹幕服务器
        let mut params = HashMap::new();
        params.insert("id".to_string(), room_info.data.room_id.to_string());
        params.insert("type".to_string(), "0".to_string());
        self.wbi_signer.add_wbi_sign(&mut params).await;

        let danmu_info: DanmuInfoResponse = self.client
            .get(DANMAKU_SERVER_CONF_URL)
            .query(&params)
            .send().await?
            .json().await?;
        
        *self.token.write().await = danmu_info.data.token;
        if let Some(server) = danmu_info.data.host_list.first() {
            *self.ws_url.write().await = format!("wss://{}:{}/sub", server.host, server.wss_port);
        }

        Ok(())
    }

    fn make_packet(operation: u32, body: &[u8]) -> Vec<u8> {
        let header_len = 16;
        let pack_len = header_len + body.len() as u32;
        let mut packet = vec![0; pack_len as usize];
        
        BigEndian::write_u32(&mut packet[0..4], pack_len);
        BigEndian::write_u16(&mut packet[4..6], header_len as u16);
        BigEndian::write_u16(&mut packet[6..8], 1);
        BigEndian::write_u32(&mut packet[8..12], operation);
        BigEndian::write_u32(&mut packet[12..16], 1);
        
        packet[16..].copy_from_slice(body);
        packet
    }

    async fn make_auth_packet(&self) -> Vec<u8> {
        let auth_body = json!({
            "uid": 0,
            "roomid": *self.real_room_id.read().await,
            "protover": 3,
            "platform": "web",
            "type": 2,
            "key": *self.token.read().await,
            "buvid": *self.buvid.read().await,
        });
        Self::make_packet(Operation::Auth as u32, serde_json::to_vec(&auth_body).unwrap().as_slice())
    }

    // 解析消息
    fn parse_messages(&self, data: &[u8]) {
        let mut offset = 0;
        
        while offset + 16 <= data.len() {
            // 解析头部
            let pack_len = BigEndian::read_u32(&data[offset..offset + 4]);
            let header_len = BigEndian::read_u16(&data[offset + 4..offset + 6]);
            let ver = BigEndian::read_u16(&data[offset + 6..offset + 8]);
            let operation = BigEndian::read_u32(&data[offset + 8..offset + 12]);
            
            if pack_len as usize > data.len() - offset {
                warn!("包长度超出数据范围");
                break;
            }
            
            let body_offset = offset + header_len as usize;
            let body_len = pack_len as usize - header_len as usize;
            
            if body_offset + body_len > data.len() {
                warn!("消息体超出数据范围");
                break;
            }
            
            let body = &data[body_offset..body_offset + body_len];
            
            match operation {
                // 业务消息
                5 => {
                    match ver {
                        // Brotli压缩
                        3 => {
                            use brotli::Decompressor;
                            use std::io::Read;
                            
                            let mut decompressor = Decompressor::new(body, 4096);
                            let mut decompressed = Vec::new();
                            match decompressor.read_to_end(&mut decompressed) {
                                Ok(_) => {
                                    info!("📦 解压Brotli数据: {} -> {} bytes", body_len, decompressed.len());
                                    self.parse_messages(&decompressed);
                                }
                                Err(e) => {
                                    error!("❌ Brotli解压失败: {:?}", e);
                                }
                            }
                        }
                        // Zlib压缩
                        2 => {
                            let mut decoder = ZlibDecoder::new(body);
                            let mut decompressed = Vec::new();
                            if decoder.read_to_end(&mut decompressed).is_ok() {
                                info!("📦 解压Zlib数据: {} -> {} bytes", body_len, decompressed.len());
                                self.parse_messages(&decompressed);
                            }
                        }
                        // 普通JSON
                        _ => {
                            if let Ok(text) = std::str::from_utf8(body) {
                                info!("💬 消息: {}", text);
                                
                                // 尝试解析JSON
                                if let Ok(json) = serde_json::from_str::<serde_json::Value>(text) {
                                    let cmd = json["cmd"].as_str().unwrap_or("UNKNOWN");
                                    
                                    // 处理不同类型的消息
                                    match cmd {
                                        "DANMU_MSG" => {
                                            if let Some(info) = json.get("info") {
                                                if let (Some(msg), Some(user_info)) = (info.get(1), info.get(2)) {
                                                    let content = msg.as_str().unwrap_or("");
                                                    let username = user_info.get(1).and_then(|u| u.as_str()).unwrap_or("匿名");
                                                    println!("📝 [弹幕] {}: {}", username, content);
                                                }
                                            }
                                        }
                                        s if s.starts_with("SEND_GIFT") => {
                                            if let Some(data) = json.get("data") {
                                                let uname = data["uname"].as_str().unwrap_or("匿名");
                                                let gift_name = data["giftName"].as_str().unwrap_or("未知礼物");
                                                let num = data["num"].as_u64().unwrap_or(0);
                                                println!("🎁 [礼物] {} 送出 {} x{}", uname, gift_name, num);
                                            }
                                        }
                                        "INTERACT_WORD" => {
                                            if let Some(data) = json.get("data") {
                                                let uname = data["uname"].as_str().unwrap_or("匿名");
                                                println!("👋 [进入] {} 进入直播间", uname);
                                            }
                                        }
                                        "ONLINE_RANK_COUNT" => {
                                            if let Some(data) = json.get("data") {
                                                let count = data["count"].as_u64().unwrap_or(0);
                                                println!("👥 [在线] 当前在线人数: {}", count);
                                            }
                                        }
                                        _ => {
                                            // 其他消息类型
                                            println!("📌 [{}] {}", cmd, json);
                                        }
                                    }
                                }
                            }
                        }
                    }
                }
                // 认证回复
                8 => info!("✅ 认证成功"),
                // 心跳回复
                3 => {
                    if body.len() >= 4 {
                        let popularity = BigEndian::read_u32(body);
                        info!("❤️ 人气值: {}", popularity);
                    }
                }
                _ => {}
            }
            
            offset += pack_len as usize;
        }
    }

    async fn run(&self) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
        loop {
            if let Err(e) = self.connect_and_listen().await {
                error!("连接错误: {}", e);
                tokio::time::sleep(Duration::from_secs(3)).await;
            }
        }
    }

    async fn connect_and_listen(&self) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
        let ws_url = self.ws_url.read().await.clone();
        info!("连接到: {}", ws_url);
        
        let (ws_stream, _) = tokio_tungstenite::connect_async(&ws_url).await?;
        let (mut write, mut read) = ws_stream.split();
        
        // 发送认证
        write.send(Message::Binary(self.make_auth_packet().await)).await?;
        
        // 心跳定时器
        let mut heartbeat_interval = tokio::time::interval(Duration::from_secs(30));
        
        loop {
            tokio::select! {
                _ = heartbeat_interval.tick() => {
                    write.send(Message::Binary(Self::make_packet(Operation::Heartbeat as u32, &[]))).await?;
                }
                Some(msg) = read.next() => {
                    match msg? {
                        Message::Binary(data) => {
                            self.parse_messages(&data);
                        }
                        Message::Close(_) => {
                            warn!("连接关闭");
                            break;
                        }
                        _ => {}
                    }
                }
            }
        }
        
        Ok(())
    }
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    // 初始化日志
    env_logger::Builder::from_env(env_logger::Env::default().default_filter_or("info")).init();
    
    // 从命令行参数获取房间号，如果没有提供则使用默认值
    let args: Vec<String> = std::env::args().collect();
    let room_id: u32 = if args.len() > 1 {
        args[1].parse().unwrap_or_else(|_| {
            eprintln!("无效的房间号，使用默认值 1111111111");
            1111111111
        })
    } else {
        println!("使用方法: {} <房间号>", args[0]);
        println!("未提供房间号，使用默认值 1111111111");
        1111111111
    };
    
    println!("🚀 启动B站弹幕监听客户端，房间号: {}", room_id);
    
    let client = Arc::new(BiliBiliClient::new(room_id));
    
    // 初始化客户端
    if let Err(e) = client.init().await {
        error!("客户端初始化失败: {}", e);
        return Err(e);
    }
    
    // 运行客户端
    if let Err(e) = client.run().await {
        error!("客户端运行出错: {}", e);
        return Err(e);
    }
    
    Ok(())
}