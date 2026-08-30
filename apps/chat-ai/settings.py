"""枠組み全体で共有する設定。

ここに置くのは「二箇所以上から要るもの」だけ。個別の設定は使う側に置く
（app.py の LLM まわり、weather.py の情報源など）。

⚠ **空文字は「未設定」として扱う。** compose から `${VAR:-}` と素通しできるように。
   空のまま Path("") にすると `.` になり、内容ファイルが見つからず起動が止まる。
"""

import os
from pathlib import Path

# 配る物の版。**ここが正**。CHANGELOG.md の見出しと必ず揃えること。
#
# ⚠ 前段(Go)にも同じ値がある（main.go の version）。
#   合っているかは起動時に照らさない——照らすには互いを呼ぶ必要があり、
#   起動の順番に依存する脆い確認になる。代わりに /healthz の両方に出しておき、
#   食い違っていれば見て分かるようにしてある。
VERSION = "0.1.3"

# 内容ファイル（人格・回答・画面の文言・顔）の置き場所。
# 既定はこのファイルの隣の content/。書き方は content/README.md。
_content = os.getenv("CHAT_CONTENT_DIR", "").strip()
CONTENT_DIR = Path(_content) if _content else Path(__file__).parent / "content"

# 外部（気象庁・Open-Meteo・Wikipedia）を叩くときの名乗り。
#
# ⚠ **配るなら必ず書き換えること。** Wikipedia は規約で UA の明示を求めており、
#   気象庁のデータも自社サイト用のものを分けてもらっている立場にある。
#   よそへ置いた設置が lab.4510.be を名乗ったままだと、
#   困りごとの問い合わせが無関係な所へ飛ぶ。
USER_AGENT = os.getenv(
    "CHAT_USER_AGENT", "lab.4510.be chat demo (+https://lab.4510.be/chat/)"
).strip() or "chat (self-hosted)"

HEADERS = {"User-Agent": USER_AGENT}
