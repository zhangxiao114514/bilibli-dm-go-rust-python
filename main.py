# -*- coding: utf-8 -*-
"""
单文件版本的B站弹幕获取工具
整合了所有必要的功能，获取弹幕和礼物信息
"""
import asyncio
import datetime
import enum
import hashlib
import json
import struct
import urllib.parse
import weakref
import zlib
from typing import Optional, List, Union, NamedTuple, Awaitable

import aiohttp
import brotli
import yarl

# 常量
USER_AGENT = (
    'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/102.0.0.0 Safari/537.36'
)

UID_INIT_URL = 'https://api.bilibili.com/x/web-interface/nav'
WBI_INIT_URL = UID_INIT_URL
BUVID_INIT_URL = 'https://www.bilibili.com/'
ROOM_INIT_URL = 'https://api.live.bilibili.com/room/v1/Room/get_info'
DANMAKU_SERVER_CONF_URL = 'https://api.live.bilibili.com/xlive/web-room/v1/index/getDanmuInfo'
DEFAULT_DANMAKU_SERVER_LIST = [
    {'host': 'broadcastlv.chat.bilibili.com', 'port': 2243, 'wss_port': 443, 'ws_port': 2244}
]

HEADER_STRUCT = struct.Struct('>I2H2I')

# 数据结构
class HeaderTuple(NamedTuple):
    pack_len: int
    raw_header_size: int
    ver: int
    operation: int
    seq_id: int

class ProtoVer(enum.IntEnum):
    NORMAL = 0
    HEARTBEAT = 1
    DEFLATE = 2
    BROTLI = 3

class Operation(enum.IntEnum):
    HANDSHAKE = 0
    HANDSHAKE_REPLY = 1
    HEARTBEAT = 2
    HEARTBEAT_REPLY = 3
    SEND_MSG = 4
    SEND_MSG_REPLY = 5
    DISCONNECT_REPLY = 6
    AUTH = 7
    AUTH_REPLY = 8
    RAW = 9
    PROTO_READY = 10
    PROTO_FINISH = 11
    CHANGE_ROOM = 12
    CHANGE_ROOM_REPLY = 13
    REGISTER = 14
    REGISTER_REPLY = 15
    UNREGISTER = 16
    UNREGISTER_REPLY = 17

class AuthReplyCode(enum.IntEnum):
    OK = 0
    TOKEN_ERROR = -101

# WBI签名器
class _WbiSigner:
    WBI_KEY_INDEX_TABLE = [
        46, 47, 18, 2, 53, 8, 23, 32, 15, 50, 10, 31, 58, 3, 45, 35,
        27, 43, 5, 49, 33, 9, 42, 19, 29, 28, 14, 39, 12, 38, 41, 13
    ]
    WBI_KEY_TTL = datetime.timedelta(hours=11, minutes=59, seconds=30)

    def __init__(self, session: aiohttp.ClientSession):
        self._session = session
        self._wbi_key = ''
        self._refresh_future: Optional[Awaitable] = None
        self._last_refresh_time: Optional[datetime.datetime] = None

    @property
    def wbi_key(self):
        return self._wbi_key

    def reset(self):
        self._wbi_key = ''
        self._last_refresh_time = None

    @property
    def need_refresh_wbi_key(self):
        return self._wbi_key == '' or (
            self._last_refresh_time is not None
            and datetime.datetime.now() - self._last_refresh_time >= self.WBI_KEY_TTL
        )

    def refresh_wbi_key(self) -> Awaitable:
        if self._refresh_future is None:
            self._refresh_future = asyncio.create_task(self._do_refresh_wbi_key())
            def on_done(_fu):
                self._refresh_future = None
            self._refresh_future.add_done_callback(on_done)
        return self._refresh_future

    async def _do_refresh_wbi_key(self):
        wbi_key = await self._get_wbi_key()
        if wbi_key == '':
            return
        self._wbi_key = wbi_key
        self._last_refresh_time = datetime.datetime.now()

    async def _get_wbi_key(self):
        async with self._session.get(
            WBI_INIT_URL,
            headers={'User-Agent': USER_AGENT},
        ) as res:
            data = await res.json()

        wbi_img = data['data']['wbi_img']
        img_key = wbi_img['img_url'].rpartition('/')[2].partition('.')[0]
        sub_key = wbi_img['sub_url'].rpartition('/')[2].partition('.')[0]

        shuffled_key = img_key + sub_key
        wbi_key = []
        for index in self.WBI_KEY_INDEX_TABLE:
            if index < len(shuffled_key):
                wbi_key.append(shuffled_key[index])
        return ''.join(wbi_key)

    def add_wbi_sign(self, params: dict):
        if self._wbi_key == '':
            return params

        wts = str(int(datetime.datetime.now().timestamp()))
        params_to_sign = {**params, 'wts': wts}

        params_to_sign = {
            key: params_to_sign[key]
            for key in sorted(params_to_sign.keys())
        }
        for key, value in params_to_sign.items():
            value = ''.join(
                ch for ch in str(value) if ch not in "!'()*"
            )
            params_to_sign[key] = value

        str_to_sign = urllib.parse.urlencode(params_to_sign) + self._wbi_key
        w_rid = hashlib.md5(str_to_sign.encode('utf-8')).hexdigest()
        return {
            **params,
            'wts': wts,
            'w_rid': w_rid
        }

# 简单的回调函数
def on_gift(gift_data):
    """处理礼物消息"""
    print(f' {gift_data["face"]} 赠送{gift_data["giftName"]}x{gift_data["num"]}')

# 全局WBI签名器缓存
_session_to_wbi_signer = weakref.WeakKeyDictionary()

def _get_wbi_signer(session: aiohttp.ClientSession) -> _WbiSigner:
    wbi_signer = _session_to_wbi_signer.get(session, None)
    if wbi_signer is None:
        wbi_signer = _session_to_wbi_signer[session] = _WbiSigner(session)
    return wbi_signer

def make_constant_retry_policy(interval: float):
    def get_interval(_retry_count: int, _total_retry_count: int):
        return interval
    return get_interval

# 主客户端类
class BiliBiliClient:
    """B站弹幕客户端"""

    def __init__(
        self,
        room_id: int,
        *,
        uid: Optional[int] = None,
        session: Optional[aiohttp.ClientSession] = None,
        heartbeat_interval=30,
    ):
        if session is None:
            self._session = aiohttp.ClientSession(timeout=aiohttp.ClientTimeout(total=10))
            self._own_session = True
        else:
            self._session = session
            self._own_session = False

        self._heartbeat_interval = heartbeat_interval
        self._wbi_signer = _get_wbi_signer(self._session)
        self._tmp_room_id = room_id
        self._uid = uid
        self._need_init_room = True
        self._get_reconnect_interval = make_constant_retry_policy(1)

        self._room_id: Optional[int] = None
        self._room_owner_uid: Optional[int] = None
        self._host_server_list: Optional[List[dict]] = None
        self._host_server_token: Optional[str] = None

        self._websocket: Optional[aiohttp.ClientWebSocketResponse] = None
        self._network_future: Optional[asyncio.Future] = None
        self._heartbeat_timer_handle: Optional[asyncio.TimerHandle] = None

    @property
    def is_running(self) -> bool:
        return self._network_future is not None

    @property
    def room_id(self) -> Optional[int]:
        return self._room_id

    @property
    def tmp_room_id(self) -> int:
        return self._tmp_room_id

    def start(self):
        """启动客户端"""
        if self.is_running:
            return
        self._network_future = asyncio.create_task(self._network_coroutine_wrapper())

    def stop(self):
        """停止客户端"""
        if not self.is_running:
            return
        if self._network_future is not None:
            self._network_future.cancel()

    async def stop_and_close(self):
        """停止客户端并释放资源"""
        if self.is_running:
            self.stop()
            await self.join()
        await self.close()

    async def join(self):
        """等待客户端停止"""
        if not self.is_running:
            return
        if self._network_future is not None:
            await asyncio.shield(self._network_future)

    async def close(self):
        """释放资源"""
        if self._own_session:
            await self._session.close()

    async def init_room(self) -> bool:
        """初始化房间信息"""
        if self._uid is None:
            await self._init_uid()

        if self._get_buvid() == '':
            await self._init_buvid()

        await self._init_room_id_and_owner()
        await self._init_host_server()
        return True

    async def _init_uid(self):
        cookies = self._session.cookie_jar.filter_cookies(yarl.URL(UID_INIT_URL))
        sessdata_cookie = cookies.get('SESSDATA', None)
        if sessdata_cookie is None or sessdata_cookie.value == '':
            self._uid = 0
            return True

        async with self._session.get(
            UID_INIT_URL,
            headers={'User-Agent': USER_AGENT},
        ) as res:
            data = await res.json()
            data = data['data']
            if not data['isLogin']:
                self._uid = 0
            else:
                self._uid = data['mid']
            return True

    def _get_buvid(self):
        cookies = self._session.cookie_jar.filter_cookies(yarl.URL(BUVID_INIT_URL))
        buvid_cookie = cookies.get('buvid3', None)
        if buvid_cookie is None:
            return ''
        return buvid_cookie.value

    async def _init_buvid(self):
        async with self._session.get(
            BUVID_INIT_URL,
            headers={'User-Agent': USER_AGENT},
        ):
            pass
        buvid = self._get_buvid()  # 添加这一行
        print(f"获取到的buvid值: {buvid}")  # 添加这一行打印
        return buvid != ''

    async def _init_room_id_and_owner(self):
        async with self._session.get(
            ROOM_INIT_URL,
            headers={'User-Agent': USER_AGENT},
            params={'room_id': self._tmp_room_id},
        ) as res:
            data = await res.json()
            self._room_id = data['data']['room_id']
            self._room_owner_uid = data['data']['uid']
        return True

    async def _init_host_server(self):
        if self._wbi_signer.need_refresh_wbi_key:
            await self._wbi_signer.refresh_wbi_key()

        async with self._session.get(
            DANMAKU_SERVER_CONF_URL,
            headers={'User-Agent': USER_AGENT},
            params=self._wbi_signer.add_wbi_sign({
                'id': self._room_id,
                'type': 0
            }),
        ) as res:
            data = await res.json()
            if data['code'] == -352:
                self._wbi_signer.reset()
            self._host_server_list = data['data']['host_list']
            self._host_server_token = data['data']['token']
        return True

    @staticmethod
    def _make_packet(data: Union[dict, str, bytes], operation: int) -> bytes:
        """创建要发送的数据包"""
        if isinstance(data, dict):
            body = json.dumps(data).encode('utf-8')
        elif isinstance(data, str):
            body = data.encode('utf-8')
        else:
            body = data
        header = HEADER_STRUCT.pack(*HeaderTuple(
            pack_len=HEADER_STRUCT.size + len(body),
            raw_header_size=HEADER_STRUCT.size,
            ver=1,
            operation=operation,
            seq_id=1
        ))
        return header + body

    async def _network_coroutine_wrapper(self):
        """网络协程包装器"""
        try:
            await self._network_coroutine()
        except asyncio.CancelledError:
            pass
        finally:
            self._network_future = None

    async def _network_coroutine(self):
        """网络协程"""
        retry_count = 0
        total_retry_count = 0
        while True:
            await self._on_before_ws_connect(retry_count)

            async with self._session.ws_connect(
                self._get_ws_url(retry_count),
                headers={'User-Agent': USER_AGENT},
                receive_timeout=self._heartbeat_interval + 5,
            ) as websocket:
                self._websocket = websocket
                await self._on_ws_connect()

                message: aiohttp.WSMessage
                async for message in websocket:
                    await self._on_ws_message(message)
                    retry_count = 0

            self._websocket = None
            await self._on_ws_close()

            retry_count += 1
            total_retry_count += 1
            await asyncio.sleep(self._get_reconnect_interval(retry_count, total_retry_count))


    async def _on_before_ws_connect(self, retry_count):
        """WebSocket连接前的准备"""
        if not self._need_init_room:
            return

        await self.init_room()
        self._need_init_room = False

    def _get_ws_url(self, retry_count) -> str:
        """获取WebSocket连接URL"""
        if self._host_server_list is None:
            return ""
        host_server = self._host_server_list[retry_count % len(self._host_server_list)]
        return f"wss://{host_server['host']}:{host_server['wss_port']}/sub"

    async def _on_ws_connect(self):
        """WebSocket连接成功"""
        await self._send_auth()
        self._heartbeat_timer_handle = asyncio.get_running_loop().call_later(
            self._heartbeat_interval, self._on_send_heartbeat
        )

    async def _on_ws_close(self):
        """WebSocket连接断开"""
        if self._heartbeat_timer_handle is not None:
            self._heartbeat_timer_handle.cancel()
            self._heartbeat_timer_handle = None

    async def _send_auth(self):
        """发送认证包"""
        auth_params = {
            'uid': self._uid,
            'roomid': self._room_id,
            'protover': 3,
            'platform': 'web',
            'type': 2,
            'buvid': self._get_buvid(),
        }
        buvid = self._get_buvid()  # 添加这一行
        print(f"发送认证时的buvid值: {buvid}")
        if self._host_server_token is not None:
            auth_params['key'] = self._host_server_token
        if self._websocket is not None:
            await self._websocket.send_bytes(self._make_packet(auth_params, Operation.AUTH))

    def _on_send_heartbeat(self):
        """发送心跳包回调"""
        if self._websocket is None or self._websocket.closed:
            self._heartbeat_timer_handle = None
            return

        self._heartbeat_timer_handle = asyncio.get_running_loop().call_later(
            self._heartbeat_interval, self._on_send_heartbeat
        )
        asyncio.create_task(self._send_heartbeat())

    async def _send_heartbeat(self):
        """发送心跳包"""
        if self._websocket is None or self._websocket.closed:
            return
        await self._websocket.send_bytes(self._make_packet({}, Operation.HEARTBEAT))


    async def _on_ws_message(self, message: aiohttp.WSMessage):
        """收到WebSocket消息"""
        if message.type == aiohttp.WSMsgType.BINARY:
            await self._parse_ws_message(message.data)


    async def _parse_ws_message(self, data: bytes):
        """解析WebSocket消息"""
        offset = 0
        header = HeaderTuple(*HEADER_STRUCT.unpack_from(data, offset))

        if header.operation in (Operation.SEND_MSG_REPLY, Operation.AUTH_REPLY):
            while True:
                body = data[offset + header.raw_header_size: offset + header.pack_len]
                await self._parse_business_message(header, body)

                offset += header.pack_len
                if offset >= len(data):
                    break
                header = HeaderTuple(*HEADER_STRUCT.unpack_from(data, offset))

    async def _parse_business_message(self, header: HeaderTuple, body: bytes):
        """解析业务消息"""
        if header.operation == Operation.SEND_MSG_REPLY:
            if header.ver == ProtoVer.BROTLI:
                body = await asyncio.get_running_loop().run_in_executor(None, brotli.decompress, body)
                await self._parse_ws_message(body)
            elif header.ver == ProtoVer.DEFLATE:
                body = await asyncio.get_running_loop().run_in_executor(None, zlib.decompress, body)
                await self._parse_ws_message(body)
            elif header.ver == ProtoVer.NORMAL:
                if len(body) != 0:
                    body = json.loads(body.decode('utf-8'))
                    self._handle_command(body)

        elif header.operation == Operation.AUTH_REPLY:
            if self._websocket is not None:
                await self._websocket.send_bytes(self._make_packet({}, Operation.HEARTBEAT))


    def _handle_command(self, command: dict):
        """处理业务消息"""
        print(f"收到消息: {json.dumps(command, ensure_ascii=False, indent=2)}")

        cmd = command.get('cmd', '')
        pos = cmd.find(':')
        if pos != -1:
            cmd = cmd[:pos]
        
        if cmd == 'SEND_GIFT':
            on_gift(command['data'])


# 使用示例
async def main():
    # 请输入正确的直播间id
    room_id = 1111111111

    # 创建客户端
    client = BiliBiliClient(room_id)

    # 启动客户端
    client.start()
    
    await client.join()
    

if __name__ == '__main__':
    asyncio.run(main())