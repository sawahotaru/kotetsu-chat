"""天気の取得。

国内と海外で情報源が違う:

  日本   : 気象庁の予報 JSON。「晴れ 夕方 から くもり」という**日本語の予報文がそのまま**返る。
           正式公開の WebAPI ではないが、政府標準利用規約に準拠して利用できる。
  海外   : Open-Meteo（キー不要・無料）。緯度経度で引く。
           ⚠ Open-Meteo のジオコーディングは**日本語の地名を受け付けない**（「東京」で0件）。
             なので都市名→座標の対訳表を自前で持つ。

どちらも落ちることがある。外部が死んでもチャット全体は動き続けるべきなので、
例外は投げずに「取得できなかった」という文字列を返す。

---- 日本の外へ配るとき ----

既定は日本前提（国内は気象庁、海外は主要都市のみ）。切り離す口が二つある:

  WEATHER_SOURCE=open-meteo   気象庁を使わず、すべて Open-Meteo で引く
  content/places.yaml         この設置で引ける地名を足す（下記）

`places.yaml` は内容ファイルの隣に置く。Open-Meteo のジオコーディングは
日本語の地名を通さないので、座標を自分で書く形にしてある:

    ミュンヘン: [48.1374, 11.5755, Europe/Berlin]
    Munich:     [48.1374, 11.5755, Europe/Berlin]   # 呼び方ごとに書いてよい

⚠ **これは情報源の切り離しであって、多言語化ではない。**
   天気の言い表し（「くもり」「明後日」）はこのファイルの日本語のまま。
   別の言語で配るなら WMO と DAY_WORDS も直す必要がある。
"""

import logging
import os
import re
import time

import httpx
import yaml

import settings
from areas import JMA_AREAS

log = logging.getLogger("chat-ai.weather")

JMA_FORECAST = "https://www.jma.go.jp/bosai/forecast/data/forecast/{code}.json"
OPEN_METEO = "https://api.open-meteo.com/v1/forecast"

# 情報源。既定の auto は「国内は気象庁、それ以外は Open-Meteo」＝従来どおり。
# open-meteo にすると気象庁を一切使わない（日本の外へ配るとき）。
SOURCE = os.getenv("WEATHER_SOURCE", "auto").strip().lower() or "auto"
if SOURCE not in ("auto", "open-meteo"):
    raise SystemExit(
        f"設定エラー: WEATHER_SOURCE={SOURCE!r} は auto か open-meteo にしてください"
    )

TIMEOUT = httpx.Timeout(12.0, connect=6.0)

# 取得した予報を持っておく時間。
# 気象庁の予報は1日3回（5時・11時・17時頃）の更新なので、毎回取りに行く必要が無い。
# 相手は正式な公開APIではない先なので、こちらが行儀よく振る舞う責任がある。
# ついでに応答も速くなる（実測 0.15秒 → 0.00秒）。
CACHE_TTL_SECONDS = int(os.getenv("WEATHER_CACHE_SECONDS", "900"))

# key -> (取得時刻, 本文)
_cache: dict[str, tuple[float, str]] = {}


def _cached(key: str) -> str | None:
    hit = _cache.get(key)
    if hit is None:
        return None
    fetched, value = hit
    if time.monotonic() - fetched > CACHE_TTL_SECONDS:
        del _cache[key]
        return None
    return value


def _store(key: str, value: str) -> str:
    # 地名の数だけしか増えないが、際限なく持つ理由も無いので上限を設ける。
    if len(_cache) > 200:
        _cache.clear()
    _cache[key] = (time.monotonic(), value)
    return value


# 気象庁は自社サイト用のデータなので、素性の分かる UA を付けて礼儀を通す。
# ⚠ 配るなら CHAT_USER_AGENT を書き換えること（settings.py の注意を参照）。
HEADERS = settings.HEADERS

# 海外の主要都市。Open-Meteo のジオコーディングが日本語を通さないため自前で持つ。
WORLD_CITIES = {
    "ロンドン": (51.5074, -0.1278, "Europe/London"),
    "パリ": (48.8566, 2.3522, "Europe/Paris"),
    "ベルリン": (52.52, 13.405, "Europe/Berlin"),
    "ローマ": (41.9028, 12.4964, "Europe/Rome"),
    "マドリード": (40.4168, -3.7038, "Europe/Madrid"),
    "モスクワ": (55.7558, 37.6173, "Europe/Moscow"),
    "ニューヨーク": (40.7128, -74.006, "America/New_York"),
    "ロサンゼルス": (34.0522, -118.2437, "America/Los_Angeles"),
    "サンフランシスコ": (37.7749, -122.4194, "America/Los_Angeles"),
    "シカゴ": (41.8781, -87.6298, "America/Chicago"),
    "バンクーバー": (49.2827, -123.1207, "America/Vancouver"),
    "トロント": (43.6532, -79.3832, "America/Toronto"),
    "メキシコシティ": (19.4326, -99.1332, "America/Mexico_City"),
    "サンパウロ": (-23.5505, -46.6333, "America/Sao_Paulo"),
    "ホノルル": (21.3069, -157.8583, "Pacific/Honolulu"),
    "シドニー": (-33.8688, 151.2093, "Australia/Sydney"),
    "メルボルン": (-37.8136, 144.9631, "Australia/Melbourne"),
    "オークランド": (-36.8485, 174.7633, "Pacific/Auckland"),
    "ソウル": (37.5665, 126.978, "Asia/Seoul"),
    "釜山": (35.1796, 129.0756, "Asia/Seoul"),
    "北京": (39.9042, 116.4074, "Asia/Shanghai"),
    "上海": (31.2304, 121.4737, "Asia/Shanghai"),
    "香港": (22.3193, 114.1694, "Asia/Hong_Kong"),
    "台北": (25.033, 121.5654, "Asia/Taipei"),
    "シンガポール": (1.3521, 103.8198, "Asia/Singapore"),
    "バンコク": (13.7563, 100.5018, "Asia/Bangkok"),
    "ハノイ": (21.0285, 105.8542, "Asia/Ho_Chi_Minh"),
    "ホーチミン": (10.8231, 106.6297, "Asia/Ho_Chi_Minh"),
    "ジャカルタ": (-6.2088, 106.8456, "Asia/Jakarta"),
    "マニラ": (14.5995, 120.9842, "Asia/Manila"),
    "デリー": (28.6139, 77.209, "Asia/Kolkata"),
    "ドバイ": (25.2048, 55.2708, "Asia/Dubai"),
    "イスタンブール": (41.0082, 28.9784, "Europe/Istanbul"),
    "カイロ": (30.0444, 31.2357, "Africa/Cairo"),
    "ケープタウン": (-33.9249, 18.4241, "Africa/Johannesburg"),
}

def _load_extra_places() -> dict:
    """内容ファイルの隣の places.yaml から、この設置で引ける地名を足す。

    無ければ何もしない（日本向けの設置では要らない）。
    書式は `名前: [緯度, 経度, タイムゾーン]`。

    ⚠ 読めない書き方をしていたら黙って飛ばさず止める。
      「書いたのに引けない」を、使った人が困るまで気づけない形にしないため。
    """
    path = settings.CONTENT_DIR / "places.yaml"
    if not path.is_file():
        return {}
    try:
        with path.open(encoding="utf-8") as f:
            raw = yaml.safe_load(f) or {}
    except yaml.YAMLError as e:
        raise SystemExit(f"設定エラー: {path} を読めません: {e}") from None
    if not isinstance(raw, dict):
        raise SystemExit(f"設定エラー: {path} は 名前: [緯度, 経度, タイムゾーン] の形にしてください")

    places = {}
    for name, value in raw.items():
        if not (isinstance(value, (list, tuple)) and len(value) == 3):
            raise SystemExit(
                f"設定エラー: {path} の {name!r} は [緯度, 経度, タイムゾーン] の3つで書いてください"
            )
        lat, lon, tz = value
        try:
            lat, lon = float(lat), float(lon)
        except (TypeError, ValueError):
            raise SystemExit(f"設定エラー: {path} の {name!r} の緯度経度が数値ではありません") from None
        if not (-90 <= lat <= 90 and -180 <= lon <= 180):
            raise SystemExit(f"設定エラー: {path} の {name!r} の緯度経度が範囲の外です")
        places[str(name)] = (lat, lon, str(tz))
    log.info("設置ごとの地名を %d 件読み込んだ（%s）", len(places), path)
    return places


# 同じ名前があれば設置側を採る（この設置の呼び方のほうが正しい）。
WORLD_CITIES.update(_load_extra_places())


# WMO の天気コード（Open-Meteo が返す）を日本語へ。
WMO = {
    0: "快晴", 1: "晴れ", 2: "晴れ時々くもり", 3: "くもり",
    45: "霧", 48: "霧（着氷性）",
    51: "弱い霧雨", 53: "霧雨", 55: "強い霧雨",
    56: "着氷性の霧雨", 57: "強い着氷性の霧雨",
    61: "弱い雨", 63: "雨", 65: "強い雨",
    66: "着氷性の雨", 67: "強い着氷性の雨",
    71: "弱い雪", 73: "雪", 75: "強い雪", 77: "霧雪",
    80: "にわか雨", 81: "強いにわか雨", 82: "激しいにわか雨",
    85: "にわか雪", 86: "強いにわか雪",
    95: "雷雨", 96: "雷雨（ひょうを伴う）", 99: "激しい雷雨（ひょうを伴う）",
}

# 「明日の天気」「あさっては？」を拾う
DAY_WORDS = [
    (("明後日", "あさって"), 2),
    (("明日", "あした", "あす"), 1),
    (("今日", "きょう", "本日"), 0),
]


def wanted_day(text: str) -> int:
    for words, offset in DAY_WORDS:
        if any(w in text for w in words):
            return offset
    return 0


def find_place(text: str):
    """本文から地名を拾う。長い地名を優先する（「宮古島」を「宮古」より先に）。"""
    for name in sorted(WORLD_CITIES, key=len, reverse=True):
        if name in text:
            return name, "world"
    if SOURCE != "auto":
        # 気象庁を使わない設置。国内の地名も Open-Meteo 側でしか引けないので、
        # ここで拾ってしまうと「引けるふり」になる。
        return None, None
    for name in sorted(JMA_AREAS, key=len, reverse=True):
        # 1文字の地名（「津」など）は誤爆しやすいので、単独で書かれた時だけ拾う
        if len(name) == 1:
            if re.search(rf"(^|[のはでを\s]){re.escape(name)}([のはでを\s]|$)", text):
                return name, "jp"
        elif name in text:
            return name, "jp"
    return None, None


async def fetch_japan(place: str, day: int) -> str:
    code = JMA_AREAS[place]
    async with httpx.AsyncClient(timeout=TIMEOUT, headers=HEADERS) as client:
        resp = await client.get(JMA_FORECAST.format(code=code))
        resp.raise_for_status()
        data = resp.json()

    series = data[0]["timeSeries"][0]
    area = series["areas"][0]
    times = series["timeDefines"]
    weathers = area["weathers"]

    if day >= len(weathers):
        return f"{place}の{['今日','明日','明後日'][day]}の予報は、まだ発表されていないようです。"

    label = ["今日", "明日", "明後日"][day]
    # 気象庁の予報文は全角スペースで区切られている。読みやすいよう詰める。
    text = weathers[day].replace("　", "")
    date = times[day][:10]

    # 気温は別の timeSeries に入っている（無い地域もある）
    temp = ""
    for s in data[0]["timeSeries"]:
        if "temps" in s["areas"][0]:
            pairs = list(zip(s["timeDefines"], s["areas"][0]["temps"]))
            same_day = [v for t, v in pairs if t[:10] == date and v not in ("", None)]
            if same_day:
                nums = sorted({int(v) for v in same_day})
                temp = f"（気温 {nums[0]}〜{nums[-1]}℃）" if len(nums) > 1 else f"（気温 {nums[0]}℃）"
            break

    # 気象庁の区域名は細かく（愛知県なら「西部」、沖縄なら「本島中南部」）、
    # それだけ返すと何処の話か分からない。尋ねられた地名を主にして併記する。
    area_name = area["area"]["name"]
    where = place if (area_name in place or place in area_name) else f"{place}（{area_name}）"
    return f"{where}の{label}（{date}）は **{text}**{temp}\n\n出典: 気象庁"


async def fetch_world(place: str, day: int) -> str:
    lat, lon, tz = WORLD_CITIES[place]
    params = {
        "latitude": lat,
        "longitude": lon,
        "daily": "weather_code,temperature_2m_max,temperature_2m_min,precipitation_probability_max",
        "timezone": tz,
        "forecast_days": 3,
    }
    async with httpx.AsyncClient(timeout=TIMEOUT, headers=HEADERS) as client:
        resp = await client.get(OPEN_METEO, params=params)
        resp.raise_for_status()
        daily = resp.json()["daily"]

    if day >= len(daily["time"]):
        return f"{place}のその日の予報は取得できませんでした。"

    label = ["今日", "明日", "明後日"][day]
    desc = WMO.get(daily["weather_code"][day], "不明")
    lo = daily["temperature_2m_min"][day]
    hi = daily["temperature_2m_max"][day]
    pop = daily["precipitation_probability_max"][day]
    pop_text = f"、降水確率 {pop}%" if pop is not None else ""

    # 現地の日付を出す（日本と日付がずれる都市があるため）
    return (
        f"{place}の{label}（{daily['time'][day]} 現地）は **{desc}**、"
        f"気温 {lo}〜{hi}℃{pop_text}\n\n出典: Open-Meteo"
    )


async def answer(text: str) -> str:
    place, kind = find_place(text)
    if not place:
        known = "、".join(list(WORLD_CITIES)[:6])
        # ⚠ 引けない設置で「都道府県名で」と案内すると、案内そのものが嘘になる。
        if SOURCE == "auto":
            return (
                "どこの天気か分かりませんでした。都道府県名か主要都市名を入れてお尋ねください。\n"
                f"（国内は全都道府県、海外は {known} などに対応しています）"
            )
        return (
            "どこの天気か分かりませんでした。都市名を入れてお尋ねください。\n"
            f"（{known} などに対応しています）"
        )

    day = wanted_day(text)
    key = f"{kind}:{place}:{day}"
    hit = _cached(key)
    if hit is not None:
        return hit

    try:
        if kind == "jp":
            return _store(key, await fetch_japan(place, day))
        return _store(key, await fetch_world(place, day))
    except (httpx.HTTPError, KeyError, IndexError, ValueError) as e:
        # 外部が落ちても会話は続ける。何が起きたかは自分のログに残す。
        log.warning("天気の取得に失敗 place=%s: %s", place, e)
        src = "気象庁" if kind == "jp" else "Open-Meteo"
        return f"{place}の天気を{src}から取得できませんでした。時間をおいてお試しください。"


def register() -> dict:
    """この処理を名乗る（plugins.load_handlers から呼ばれる）。

    ここに書いた名前が intents.yaml の `handler:` と対応する。

    ⚠ 同梱してあるが**枠組みの一部ではない**。外の世界（気象庁 / Open-Meteo）に
       触るものなので、要らない設置は CHAT_HANDLERS から外せる。
       外すと、これを参照する意図は起動時に自動で落ちる。
       ただし**案内文は静的なテキストなので残る**——外すなら内容ファイルも
       一緒に差し替えること（さもないと「天気を聞かれよ」と促しておいて答えられない）。
    """
    return {"weather": answer}
