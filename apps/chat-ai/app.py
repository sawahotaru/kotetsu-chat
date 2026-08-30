"""チャットの後段（AI サービス）。

前段（Go）から渡された会話に答える。答え方は2通りある:

  1. **ルート**（intents.py）… よくある問いは意図分類して定型文や外部APIで即答する。
     LLM を起こさないので CPU をほぼ使わず、答えも常に正しい。
  2. **LLM**（ollama）… 1 で拾えなかったものだけ回す。

この順序が肝心で、**LLM は最後の手段**として置いてある。
天気・料金・技術構成のように答えが決まっているものを 1B のモデルに推測させる理由は無い
（平気で嘘をつく）。ルールは賢さの担当ではなく「よく来る質問を安く正しく捌く」担当。

外部に直接は公開されない（Caddy のルートは前段だけ）。
レート制限・同時実行制御・入力長の上限は前段（Go）の担当なのでここには置かない。
ただし「壊れた入力で 500 にしない」程度の検証はする（前段を通さず直接叩かれても落ちないように）。
"""

import asyncio
import inspect
import json
import logging
import os
import time
from contextlib import asynccontextmanager
from typing import AsyncIterator, Literal

import httpx
from fastapi import FastAPI, HTTPException
from fastapi.responses import FileResponse, StreamingResponse
from pydantic import BaseModel, Field

import intents as intent_defs
import settings
from plugins import load_handlers
from router import Router

log = logging.getLogger("chat-ai")
logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")

# ---- 設定の読み取り ----
#
# ⚠ 既定値は **compose と食い違わせないこと**。
#    以前 CHAT_LLM_ENABLED が compose 側 0・ここ 1 と逆で、
#    「コードを読むと既定オン、実際に動くとオフ」という状態になっていた。
#    実際に効くのは compose 側なので、ここは常に **安全な側（無効）** に倒す。
#
# ⚠ 読めない値は既定値で握り潰さず、その場で止める。
#    設定を間違えたまま起動してしまうと、使った人が困るまで誰も気づかない。
#    配る物では、これが最も多い苦情の元になる。

class ConfigError(SystemExit):
    """設定が読めない・値が範囲外。

    SystemExit を継承しているので、送出すると **追跡（traceback）を出さずに終了**する。
    設定の書き間違いに長い追跡を見せても、直す助けにならないため。
    """

    def __init__(self, message: str) -> None:
        super().__init__(f"設定エラー: {message}")


def _env_int(key: str, default: int, *, minimum: int | None = None) -> int:
    raw = os.getenv(key, "").strip()
    if not raw:
        return default
    try:
        value = int(raw)
    except ValueError:
        raise ConfigError(f"{key}={raw!r} は整数として読めません（既定 {default}）") from None
    if minimum is not None and value < minimum:
        raise ConfigError(f"{key}={value} は {minimum} 以上にしてください")
    return value


def _env_float(key: str, default: float, *, low: float, high: float) -> float:
    raw = os.getenv(key, "").strip()
    if not raw:
        return default
    try:
        value = float(raw)
    except ValueError:
        raise ConfigError(f"{key}={raw!r} は数値として読めません（既定 {default}）") from None
    if not low <= value <= high:
        raise ConfigError(f"{key}={value} は {low}〜{high} の範囲にしてください")
    return value


def _env_bool(key: str, default: bool) -> bool:
    raw = os.getenv(key, "").strip().lower()
    if not raw:
        return default
    if raw in ("1", "true", "yes", "on"):
        return True
    if raw in ("0", "false", "no", "off"):
        return False
    raise ConfigError(f"{key}={raw!r} は 1/0（true/false）で指定してください")


OLLAMA_URL = os.getenv("OLLAMA_URL", "http://ollama:11434")

# 使ってよいモデル。ここに無い名前は受け付けない（任意のモデルを引かせない）。
# 4コアCPU・GPU無しの前提なので 1〜3B 級に絞る。名前は環境変数で差し替えられる。
MODELS = [m.strip() for m in os.getenv("CHAT_MODELS", "gemma3:1b,qwen2.5:1.5b").split(",") if m.strip()]
DEFAULT_MODEL = os.getenv("CHAT_DEFAULT_MODEL", MODELS[0] if MODELS else "gemma3:1b")

DEFAULT_SYSTEM = os.getenv("CHAT_SYSTEM_PROMPT", intent_defs.SYSTEM_PROMPT)

# ルールで答えられる問いをルールで答える。False にすると常に LLM へ回る（比較用）。
ROUTING_ENABLED = _env_bool("CHAT_ROUTING_ENABLED", True)

# LLM を使うか。**公開版は無効**にしている。
#
# 理由は負荷ではなく中身。lab は仕事に繋げるための展示物で、そこに置いたボットが
# 見込み客の前で嘘をつく損失のほうが、「何でも答えられる」利点より大きい。
# 1B のモデルは実際に「Goは日本で開発された言語」「（実在する商品を）販売しておりません」と答えた。
#
# 無効のとき ollama は不要（compose の profile で起動対象から外してある）。
# 手元で実験するときや、相手が特定できる経路（LINE等）でだけ有効にする。
# 既定は **無効**（compose の既定と揃える。迷ったら安全な側に倒す）。
LLM_ENABLED = _env_bool("CHAT_LLM_ENABLED", False)

# 生成の上限。CPU推論なので長文を作らせると数分かかる。
NUM_PREDICT = _env_int("CHAT_NUM_PREDICT", 512, minimum=1)
NUM_CTX = _env_int("CHAT_NUM_CTX", 4096, minimum=256)
DEFAULT_TEMPERATURE = _env_float("CHAT_TEMPERATURE", 0.7, low=0.0, high=2.0)

def _validate_config() -> None:
    """項目をまたぐ整合を確かめる。読めるが噛み合っていない設定を弾く。"""
    if not OLLAMA_URL.startswith(("http://", "https://")):
        raise ConfigError(f"OLLAMA_URL={OLLAMA_URL!r} は http:// か https:// で始めてください")
    if LLM_ENABLED and not MODELS:
        raise ConfigError("CHAT_LLM_ENABLED=1 ですが CHAT_MODELS が空です。使うモデルを指定してください")
    if MODELS and DEFAULT_MODEL not in MODELS:
        raise ConfigError(
            f"CHAT_DEFAULT_MODEL={DEFAULT_MODEL!r} が CHAT_MODELS({', '.join(MODELS)}) に含まれていません"
        )
    if NUM_PREDICT >= NUM_CTX:
        raise ConfigError(
            f"CHAT_NUM_PREDICT({NUM_PREDICT}) は CHAT_NUM_CTX({NUM_CTX}) より小さくしてください"
            "（生成の上限が文脈の窓を超えると、途中で打ち切られる）"
        )


_validate_config()
log.info(
    "chat-ai %s / 設定 LLM=%s ルート=%s モデル=%s 生成上限=%d 文脈=%d",
    settings.VERSION,
    LLM_ENABLED, ROUTING_ENABLED, ",".join(MODELS) or "(なし)", NUM_PREDICT, NUM_CTX,
)

# モデルの取得状況。UI に「準備中」を出すために持つ。
# 値は "pending" | "pulling" | "ready" | "error: <理由>"
model_status: dict[str, str] = {m: "pending" for m in MODELS}


# ---- 起動時のモデル取得 ----

async def pull_model(client: httpx.AsyncClient, model: str) -> None:
    """Ollama にモデルを取得させる。既にあれば即座に終わる。

    数百MB〜数GBのダウンロードなので、起動をここで待たせない（背景で走らせる）。
    失敗しても API は上げたままにする。UI に理由を出したいので握りつぶさず記録する。
    """
    model_status[model] = "pulling"
    started = time.monotonic()
    try:
        async with client.stream(
            "POST", f"{OLLAMA_URL}/api/pull", json={"model": model}, timeout=httpx.Timeout(None)
        ) as resp:
            if resp.status_code != 200:
                body = (await resp.aread()).decode("utf-8", "replace")[:200]
                model_status[model] = f"error: {resp.status_code} {body}"
                log.error("pull 失敗 model=%s status=%s", model, resp.status_code)
                return
            # 進捗が流れてくる。最後の行に error が入ることがあるので読み切る。
            last_error = ""
            async for line in resp.aiter_lines():
                if not line.strip():
                    continue
                try:
                    ev = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if ev.get("error"):
                    last_error = str(ev["error"])
            if last_error:
                model_status[model] = f"error: {last_error}"
                log.error("pull 失敗 model=%s err=%s", model, last_error)
                return
    except Exception as e:  # noqa: BLE001 - 起動処理なので何が来ても上げ続ける
        model_status[model] = f"error: {e}"
        log.exception("pull 中に例外 model=%s", model)
        return

    model_status[model] = "ready"
    log.info("pull 完了 model=%s %.1fs", model, time.monotonic() - started)


async def pull_all() -> None:
    """モデルを1つずつ順に取得する。

    並行に落とすと 4 コアの VM では帯域もCPUも取り合いになるうえ、
    ディスク（空き25GB程度）を一気に使う。順番に確実に増やす。
    """
    if not LLM_ENABLED:
        log.info("LLM は無効。モデルの取得は行わない（ルールのみで応答する）")
        return
    async with httpx.AsyncClient() as client:
        for model in MODELS:
            await pull_model(client, model)


# ---- 意図分類 ----

# 処理の読み込みは **plugins.py に一本化**してある。
#
# 以前はここに CORE_HANDLERS（weather / wikipedia）を直に書き、設置ごとの追加だけを
# 読み込んで後ろから重ねていた。そのため:
#   - 同梱の処理を追加が**黙って上書き**できた（重複の検査を通らない）
#   - 天気も調べ物も外せず、使わない設置にも他人のサービスへの依存が載り続けた
# いまはどちらも同じ一覧に並ぶ。名前が衝突すれば止まる。
#
# 読み込まれなかった処理を参照する意図は build_intents() が起動時に外す
# （記録に残る）ので、**減らしても起動する**。
router = Router(intent_defs.build_intents(load_handlers()))


@asynccontextmanager
async def lifespan(app: FastAPI):
    task = asyncio.create_task(pull_all())
    yield
    task.cancel()


app = FastAPI(
    # ⚠ 設置の名は content から取る。ここに書くと、配った先が別の名を名乗れない。
    title=f"chat-ai | {intent_defs.NAME}",
    description="チャットの後段。Ollama を呼んで生成を NDJSON で流す。",
    root_path=os.getenv("ROOT_PATH", ""),
    lifespan=lifespan,
)


# ---- 型 ----

class Message(BaseModel):
    role: Literal["user", "assistant"]
    content: str = Field(min_length=1, max_length=8000)


class ChatRequest(BaseModel):
    messages: list[Message] = Field(min_length=1, max_length=32)
    model: str = ""
    system: str = ""
    temperature: float | None = Field(default=None, ge=0.0, le=2.0)
    # 呼び出し側（口）ごとに LLM を使うかを決める。
    #
    # 同じ頭（意図分類＋ハンドラ）を、性質の違う口が共有するための切り替え:
    #   Web（匿名・誰でも来る）      → false … ルールだけ。嘘をつかない
    #   LINE（友だち追加・相手が判る）→ true  … 拾えなければ LLM に回す
    #
    # ⚠ ブラウザからは設定できない。前段(Go)は受け取った JSON をそのまま転送せず、
    #   自分の設定値で組み直して送るので、この項目は利用者の手の届かない所にある。
    allow_llm: bool = True


# ---- エンドポイント ----

@app.get("/healthz")
async def healthz() -> dict[str, str]:
    """生きているか、と、何の版か。

    版を出すのは、配った先で「どれが動いているのか」を確かめる術が
    他に無いため（イメージのタグは付け替えられる）。
    """
    return {"status": "ok", "version": settings.VERSION}


@app.get("/persona")
async def persona() -> dict:
    """画面を組み立てるのに要るもの（表題・名乗り・質問例）を返す。

    前段(Go)に埋め込まず、ここから配る。前段は器のままにしておき、
    差し替えるのは内容ファイルだけで済むようにするため。
    """
    return intent_defs.ui_payload(LLM_ENABLED)


@app.get("/avatar")
async def avatar() -> FileResponse:
    """案内役の顔。内容ファイルの隣に置いた画像をそのまま返す。

    頁に焼き込まずここから配ることで、画像を置き換えるだけで顔が変わる。
    """
    if intent_defs.AVATAR_PATH is None:
        raise HTTPException(status_code=404, detail="顔の画像は設定されていません")
    return FileResponse(
        intent_defs.AVATAR_PATH,
        # 顔はめったに変わらない。毎回取りに来させる理由が無い。
        headers={"Cache-Control": "public, max-age=86400"},
    )


@app.get("/models")
async def models() -> dict:
    """使えるモデルと、その準備状況を返す。

    Ollama 側に実際に存在するかも確認する（pull の記録だけを信じない）。
    """
    if not LLM_ENABLED:
        # 無効時は ollama が起動していないのが正常。接続を試みて警告を出す意味が無い。
        return {
            "models": [],
            "default": "",
            "system_prompt": "",
            "ollama_reachable": False,
            "llm_enabled": False,
        }

    installed: set[str] = set()
    reachable = True
    try:
        async with httpx.AsyncClient(timeout=10) as client:
            resp = await client.get(f"{OLLAMA_URL}/api/tags")
            resp.raise_for_status()
            installed = {m["name"] for m in resp.json().get("models", [])}
    except Exception:  # noqa: BLE001 - 落ちている事実を UI に返したいだけ
        reachable = False
        log.warning("ollama に接続できない")

    items = []
    for m in MODELS:
        status = model_status.get(m, "pending")
        if status != "ready" and m in installed:
            status = "ready"  # 前回起動時などに取得済み
        items.append({"name": m, "status": status, "ready": status == "ready"})

    return {
        "models": items,
        "default": DEFAULT_MODEL,
        "system_prompt": DEFAULT_SYSTEM,
        "ollama_reachable": reachable,
        "llm_enabled": True,
    }


def build_payload(req: ChatRequest) -> dict:
    """Ollama の /api/chat に渡す形へ組み立てる。"""
    model = req.model or DEFAULT_MODEL
    if model not in MODELS:
        raise HTTPException(status_code=400, detail=f"使用できないモデルです: {model}")

    system = req.system.strip() or DEFAULT_SYSTEM
    messages = [{"role": "system", "content": system}]
    messages += [{"role": m.role, "content": m.content} for m in req.messages]

    temperature = DEFAULT_TEMPERATURE if req.temperature is None else req.temperature

    return {
        "model": model,
        "messages": messages,
        "stream": True,
        "options": {
            "temperature": temperature,
            "num_predict": NUM_PREDICT,
            "num_ctx": NUM_CTX,
        },
    }


async def generate(payload: dict) -> AsyncIterator[bytes]:
    """Ollama のストリームを NDJSON へ正規化して流す。

    前段が扱う type は3つだけ:
      token : 生成された断片
      done  : 完了（統計つき）
      error : 失敗（理由つき）
    """
    model = payload["model"]

    yield emit({"type": "status", "message": f"{model} で生成中…"})

    started = time.monotonic()
    try:
        async with httpx.AsyncClient(timeout=httpx.Timeout(None, connect=10)) as client:
            async with client.stream("POST", f"{OLLAMA_URL}/api/chat", json=payload) as resp:
                if resp.status_code != 200:
                    body = (await resp.aread()).decode("utf-8", "replace")[:300]
                    # 一番ありがちなのは pull がまだ終わっていないケース。
                    hint = "モデルの準備がまだ終わっていない可能性があります。" if resp.status_code == 404 else ""
                    yield emit({"type": "error", "message": f"生成に失敗しました（{resp.status_code}）。{hint}{body}"})
                    return

                async for line in resp.aiter_lines():
                    if not line.strip():
                        continue
                    try:
                        ev = json.loads(line)
                    except json.JSONDecodeError:
                        continue

                    if ev.get("error"):
                        yield emit({"type": "error", "message": str(ev["error"])})
                        return

                    chunk = ev.get("message", {}).get("content", "")
                    if chunk:
                        yield emit({"type": "token", "text": chunk})

                    if ev.get("done"):
                        elapsed = time.monotonic() - started
                        count = ev.get("eval_count") or 0
                        eval_ns = ev.get("eval_duration") or 0
                        tps = (count / (eval_ns / 1e9)) if count and eval_ns else None
                        yield emit({
                            "type": "done",
                            "route": "llm",
                            "model": model,
                            "elapsed_seconds": round(elapsed, 1),
                            "eval_count": count,
                            "tokens_per_second": round(tps, 1) if tps else None,
                        })
                        return
    except httpx.ReadError:
        # 前段が接続を切った（ブラウザが閉じた）。異常ではない。
        log.info("下流が切断された model=%s", model)
    except Exception as e:  # noqa: BLE001
        log.exception("生成中に例外 model=%s", model)
        yield emit({"type": "error", "message": f"生成中にエラーが発生しました: {e}"})


def emit(obj: dict) -> bytes:
    return (json.dumps(obj, ensure_ascii=False) + "\n").encode("utf-8")


async def respond_with_text(answer: str, route: str, **extra) -> AsyncIterator[bytes]:
    """出来上がった文字列を流す。画面の見え方を LLM のときと揃えるため少しずつ送る。"""
    for i in range(0, len(answer), 12):
        yield emit({"type": "token", "text": answer[i : i + 12]})
    yield emit({"type": "done", "route": route, **extra})


async def respond_with_rule(text: str, intent, score: float, llm_available: bool) -> AsyncIterator[bytes]:
    """ルールで答える。LLM は起動しない。"""
    started = time.monotonic()
    try:
        if intent.handler is not None:
            result = intent.handler(text)
            answer = await result if inspect.isawaitable(result) else result
        else:
            answer = intent.answer
        # 「LLM を使っているか」で変わる文面をここで確定させる（口ごとに違うため）
        answer = intent_defs.apply_llm_notes(answer, llm_available)
    except Exception as e:  # noqa: BLE001 - ハンドラが転んでも会話は続ける
        log.exception("ハンドラで例外 intent=%s", intent.name)
        yield emit({"type": "error", "message": f"うまく調べられませんでした: {e}"})
        return

    async for chunk in respond_with_text(
        answer,
        route="rule",
        intent=intent.name,
        confidence=round(score, 2),
        elapsed_seconds=round(time.monotonic() - started, 2),
    ):
        yield chunk


@app.post("/chat")
async def chat(req: ChatRequest) -> StreamingResponse:
    headers = {"Cache-Control": "no-store", "X-Accel-Buffering": "no"}
    last_user = req.messages[-1].content if req.messages else ""

    # 会話が続いている最中はルールに割り込ませない。
    # 「東京の天気は？」→「じゃあ大阪は？」のような続きを、単発の質問として
    # 誤って拾ってしまうため。2往復目からは LLM に文脈ごと任せる。
    # ただし LLM が無効なら渡す先が無いので、常にルートを試みる。
    first_turn = len(req.messages) == 1

    # 実効値 = サーバーが持っているか（ollamaが動いているか）× 呼び出し側が望むか
    llm_available = LLM_ENABLED and req.allow_llm

    # どちらの経路を通ったかは外から見えない。ルールが素通りされていても
    # 「LLM がそう答えた」ようにしか見えず、原因の切り分けができないので残す。
    log.info(
        "受信 messages=%d first_turn=%s allow_llm=%s llm_available=%s",
        len(req.messages), first_turn, req.allow_llm, llm_available,
    )

    if ROUTING_ENABLED and (first_turn or not llm_available):
        intent, score = router.match(last_user)
        # ⚠ 利用者の文面は記録しない（「会話は残さぬ」と答えている以上、破ってはならぬ）。
        #    残すのは判定結果だけ。閾値の調整にはこれで足りる。
        log.info("照合 intent=%s score=%.3f", intent.name if intent else None, score)
        if intent is not None:
            log.info("ルートで応答 intent=%s score=%.2f", intent.name, score)
            return StreamingResponse(
                respond_with_rule(last_user, intent, score, llm_available),
                media_type="application/x-ndjson",
                headers=headers,
            )

    if not llm_available:
        # 「分かりません」で終わらせず、答えられることを示す。
        # 何を聞けばよいか分からないまま帰らせるのが、この手のボットの一番の失敗。
        #
        # ⚠ ここに利用者の文面を出さないこと。
        #   以前は last_user[:60] を出していた。この枝は llm_available が偽のとき——
        #   つまり **公開している Web の口では毎回** 通る。しかも通るのは
        #   「規則で拾えなかった問い」＝最も踏み込んだ、その人固有の質問だけ。
        #   一方この chat-ai は、聞かれれば「会話は残さぬ」と答える。破ってはならぬ。
        #   閾値の調整には、拾えたか否かと score（上の「照合」）だけで足りる。
        log.info("ルートで拾えず（LLM無効）")
        return StreamingResponse(
            respond_with_text(intent_defs.UNKNOWN, route="none"),
            media_type="application/x-ndjson",
            headers=headers,
        )

    payload = build_payload(req)  # 不正なモデル名はここで 400 になる（ストリーム開始前）
    return StreamingResponse(
        generate(payload),
        media_type="application/x-ndjson",
        headers=headers,
    )
