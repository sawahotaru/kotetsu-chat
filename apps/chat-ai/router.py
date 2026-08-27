"""意図分類（文字N-gram の類似度）。

日本語は単語の区切りが無いので、形態素解析（MeCab 等）を入れるか、
文字N-gram で照合するかの二択になる。ここは後者を選んだ:

  - 追加の依存が要らない（scikit-learn も TensorFlow も MeCab も使わない）
  - 表記ゆれに強い。「天気」「てんき」「天氣」が部分的に一致する
  - 数十個の意図なら数ミリ秒で終わる。事前学習も不要

代わりに諦めたもの: 語順や否定の理解。「東京の天気じゃなくて大阪」は正しく扱えない。
そういう入力は閾値を割って LLM へ落ちる。**それでよい**、というのがこの設計の要点で、
ルールは「よく来る質問を安く捌く」ためにあり、賢さの担当ではない。
"""

import math
import re
import unicodedata
from collections import Counter

# この値を超えたらルールで答える。下回ったら LLM へ回す。
# 上げるとLLMに落ちる率が上がり（重くなるが安全）、下げると誤爆が増える。
MATCH_THRESHOLD = 0.34

_PUNCT = re.compile(r"[\s、。，．,.!！?？「」『』（）()\[\]【】〜~ー・:：;；\"'`]+")


def normalize(text: str) -> str:
    """全角/半角・大文字小文字・記号のゆれを潰す。"""
    t = unicodedata.normalize("NFKC", text).lower()
    return _PUNCT.sub("", t)


def ngrams(text: str) -> Counter:
    """2〜3文字のN-gram。短い語（「猫」「雨」）が消えないよう1文字も混ぜる。"""
    t = normalize(text)
    grams: Counter = Counter()
    for n in (1, 2, 3):
        if len(t) < n:
            continue
        for i in range(len(t) - n + 1):
            # 1文字だけの一致は弱い証拠なので重みを下げる
            grams[t[i : i + n]] += 1 if n > 1 else 0.4
    return grams


def cosine(a: Counter, b: Counter) -> float:
    if not a or not b:
        return 0.0
    common = set(a) & set(b)
    if not common:
        return 0.0
    dot = sum(a[g] * b[g] for g in common)
    na = math.sqrt(sum(v * v for v in a.values()))
    nb = math.sqrt(sum(v * v for v in b.values()))
    return dot / (na * nb) if na and nb else 0.0


class Intent:
    """1つの意図。

    examples : この意図に該当する言い方の例。多いほど当たりやすい。
    must     : このどれかを含まないと採用しない（誤爆止め）。空なら無条件。
    handler  : (text, match) -> str | Awaitable[str]。None なら answer をそのまま返す。
    matcher  : (text) -> bool。True なら例文の近さに関わらず最低点を保証する。

    matcher は**類似度では拾えない意図**のための逃げ道。
    調べ物（「〇〇とは」）がその典型で、判ずるべきは「例文に似ているか」ではなく
    「その言い回しに当てはまるか」である。しかも語が長いほど類似度は薄まるので、
    「織田信長とは」は 0.21 まで落ちて閾値を割っていた（実際に取りこぼした）。
    言い回しで判定できるものは、類似度に頼らず matcher で拾う。
    """

    def __init__(self, name, examples, answer=None, handler=None, must=None, priority=0,
                 matcher=None):
        self.name = name
        self.examples = examples
        self.answer = answer
        self.handler = handler
        self.must = must or []
        self.priority = priority
        self.matcher = matcher
        self._vectors = [ngrams(e) for e in examples]
        self._must_normalized = [normalize(m) for m in self.must]

    # must を満たしたときの加点。
    # must は「ゲーム」「贈」のような、その意図に固有の語を並べたもの。
    # 言い回しが例文と違っても（「ポーカーやりたい」に対して「ポーカーで遊びたい」）、
    # 固有語が入っていれば拾えるようにする。誤爆を招かない程度に小さく。
    MUST_BONUS = 0.08

    # matcher が当たったときに保証する点。
    # 閾値(0.34)より上、例文がよく似ているとき(0.5〜1.1)より下に置く。
    # こうしておくと「取りこぼさないが、より確かな意図には譲る」という振る舞いになる。
    # 例:「予約システムについて教えて」は調べ物の形だが、demo_clinic(0.94) に譲る。
    MATCHER_SCORE = 0.45

    def score(self, text: str, vec: Counter) -> float:
        s = 0.0
        # must を満たしていれば、例文との近さで測る
        if not self._must_normalized or any(
            m in normalize(text) for m in self._must_normalized
        ):
            best = max((cosine(vec, v) for v in self._vectors), default=0.0)
            if best > 0:
                s = best + (self.MUST_BONUS if self._must_normalized else 0.0)

        # matcher は must を通さない。それ自体が must より厳密な判定だから。
        if self.matcher is not None and self.matcher(text):
            s = max(s, self.MATCHER_SCORE)

        if s <= 0:
            return 0.0
        # priority は僅差の取り合いを決めるためのもの。閾値を跨がせるほどの下駄は履かせない。
        return s + 0.02 * self.priority


class Router:
    def __init__(self, intents):
        self.intents = intents

    def match(self, text: str):
        """最有力の意図と得点を返す。閾値未満なら (None, 得点)。"""
        vec = ngrams(text)
        best, best_score = None, 0.0
        for intent in self.intents:
            s = intent.score(text, vec)
            if s > best_score:
                best, best_score = intent, s
        if best_score < MATCH_THRESHOLD:
            return None, best_score
        return best, best_score
