"""意図分類の回帰テスト。

意図を足すたびに既存が壊れるので、**期待する行き先の一覧**を並べて一括で確かめる。
特に **誤爆よけ**（調べ物が lab の案内を奪わない等）を必ず含めること。
過去に踏んだ事故は消さずに残す。同じ穴を二度掘らないための記録でもある。

実行:

    docker compose exec -T chat-ai python -m pytest -q          # 動いている器で
    cd apps/chat-ai && pip install -r requirements-dev.txt && pytest -q   # 手元で

外部へは一切出ない（ハンドラは呼ばず、どの意図に落ちるかだけを見る）。

⚠ **この一覧は同梱の content（黒猫武将こてつ）に向けて書いてある。**
   自分の内容に差し替えたら、この一覧も自分のものに書き換えて使う。
   書き換える前に走らせた場合は、130件の失敗を並べる代わりに
   **理由を告げて丸ごと飛ばす**（下の _guard を参照）。
   別の内容を読んでいるだけなのか、本当に壊したのかを、出力で見分けられるように。
"""

import pytest

import intents as intent_defs
import wikipedia
from router import Router

router = Router(intent_defs.build_intents({
    # 実際には呼ばないので None でよい。matcher だけは判定に使うので本物を渡す。
    #
    # ⚠ products / clinic は本来 CHAT_SITE_HANDLERS 経由で入る「設置ごとの追加」
    #   （plugins.py 参照）。ここでは分類だけを試したいので直に登録している。
    #   ⚠ 「登録されていない」と「登録された値が None」を混同しないこと。
    #      値で判ずると、この None が未登録扱いになって意図が丸ごと消える
    #      （実際に回帰試験が15件落ちた）。
    "weather": None,
    "products": None,
    "clinic": None,
    "wikipedia": None,
    "wikipedia_matches": wikipedia.matches,
}))

def _guard() -> None:
    """いま読んでいる内容が、この一覧の向き先かどうかを確かめる。

    半分より多くの意図が見当たらなければ「別の内容を読んでいる」と判ずる。
    数件なら本当に壊した可能性のほうが高いので、いつもどおり失敗させる。
    この線引きが、**差し替えた人の混乱**と**壊した人への警告**を分ける。
    """
    defined = {it.name for it in router.intents}
    expected = {want for _, want in CASES}
    missing = expected - defined
    if len(missing) > len(expected) / 2:
        pytest.skip(
            f"この一覧は同梱の content 用です（{len(missing)}/{len(expected)} の意図が"
            f"いま読んでいる内容 {intent_defs.CONTENT_DIR} にありません）。\n"
            f"自分の内容に差し替えたなら、test_router.py の CASES も"
            f"自分のものに書き換えてください。",
            allow_module_level=True,
        )


CASES = [
    # ---- lab の案内 ----
    ("何ができますか", "lab_help"),
    ("使い方を教えて", "lab_help"),
    ("ここは何のサイトですか", "lab_about"),
    ("技術構成を教えて", "lab_tech"),
    ("無料ですか", "lab_cost"),
    ("ソースコードは公開されてますか", "lab_source"),
    ("自己紹介して", "kotetsu"),
    ("あなたは誰ですか", "kotetsu"),
    ("こんにちは", "greeting"),
    ("ありがとう", "thanks"),

    # ---- 予約システムの導線（lab の本命）----
    ("うちの院でも使えますか", "clinic_pitch"),
    ("整体院を経営しています", "clinic_pitch"),
    ("導入を検討しています", "clinic_pitch"),
    ("同じ予約システムが欲しい", "clinic_pitch"),
    ("二重予約は防げますか", "clinic_features"),
    ("リマインドメールは送れますか", "clinic_features"),
    ("営業時間は設定できますか", "clinic_features"),
    ("肩こりがひどいです", "clinic_symptom"),
    ("腰痛を診てほしい", "clinic_symptom"),
    ("整体に行きたい", "clinic_symptom"),
    ("仕事を依頼できますか", "work_request"),
    ("見積もりが欲しい", "work_request"),

    # ---- lab へのよくある問い ----
    ("誰が作ったのですか", "author"),
    ("なぜこれを作ったの", "why"),
    ("個人情報は大丈夫ですか", "privacy"),
    ("会話は保存されますか", "privacy"),
    ("スマホでも見られますか", "mobile"),
    ("連絡先を教えてください", "contact"),
    ("問い合わせはどこからですか", "contact"),
    ("何から見ればいいですか", "tour"),
    ("初めて来ました", "tour"),
    ("サーバーは何を使っていますか", "infra"),
    ("どこで動いているの", "infra"),
    ("止まったりしませんか", "reliability"),
    ("バックアップは取っていますか", "reliability"),
    ("反応が遅いです", "slow"),
    ("バグを見つけました", "bug_report"),
    ("エラーが出ます", "bug_report"),
    ("管理画面はどこですか", "admin_login"),
    ("本当にお金がかかりますか", "ec_payment"),
    ("課金されませんか", "ec_payment"),
    ("注文履歴を見たい", "ec_order"),
    ("go 言語のページはどこ", "demo_go"),
    ("python のデモを見たい", "demo_py"),

    # ---- 世間話 ----
    ("元気ですか", "how_are_you"),
    ("疲れました", "tired"),          # ←「〜ました」で thanks に流れていた
    ("暇です", "bored"),
    ("すごいね", "praise"),
    ("かわいい", "praise"),
    ("役に立たないな", "scold"),
    ("好きな食べ物は", "food"),
    ("何歳ですか", "age"),
    ("冗談を言って", "joke"),
    ("趣味は何ですか", "hobby"),
    ("今何時ですか", "time_unknown"),
    ("英語は話せますか", "language"),
    ("あなたは人間ですか", "are_you_ai"),
    ("ロボットなの", "are_you_ai"),
    ("こてつという名前の由来は", "name_origin"),
    ("落ち込んでいます", "sad"),
    ("プログラミングを学びたい", "learn"),
    ("猫を飼いたいです", "cat_adopt"),
    ("保護猫について知りたい", "cat_adopt"),
    ("猫は好きですか", "animals"),

    # ---- 動的ハンドラ ----
    ("東京の天気は", "weather"),
    ("3000円以内で贈り物ある", "product_search"),
    ("猫のグッズはある", "product_search"),
    ("予約したい", "clinic_availability"),
    ("明日空いてますか", "clinic_availability"),

    # ---- 調べ物 ----
    # 語が長いと類似度が薄まって閾値を割るため、matcher で拾っている。
    # 「織田信長とは」は 0.21 まで落ちて取りこぼしていた（実際の報告）。
    ("量子力学とは何ですか", "wikipedia"),
    ("ハチ公について教えて", "wikipedia"),
    ("アルデンテって何", "wikipedia"),
    ("フェルマーの最終定理の意味を教えて", "wikipedia"),
    ("織田信長とは", "wikipedia"),
    ("織田信長という人とは？", "wikipedia"),
    ("織田信長って何者？", "wikipedia"),
    ("織田信長って誰", "wikipedia"),
    ("織田信長は何をした人ですか", "wikipedia"),
    ("エスプレッソとはどんな飲み物ですか", "wikipedia"),

    # ---- 実際に会話して見つかった穴（95問を浴びせて65問が UNKNOWN だった回の分）----
    # 商談（最重要。「料金プランを教えて」が wikipedia に流れていた）
    ("料金プランを教えて", "pricing"),
    ("月額いくらですか", "pricing"),
    ("納期はどのくらい", "pricing"),
    ("保守もお願いできますか", "pricing"),
    ("無料で試せますか", "pricing"),
    ("実績はありますか", "track_record"),
    ("他社と何が違うんですか", "track_record"),
    ("求人はありますか", "hiring"),
    # 予約デモを触った人の困りごと
    ("予約をキャンセルしたい", "reservation_change"),
    ("予約が取れているか分からない", "reservation_change"),
    ("確認メールが届きません", "reservation_change"),
    ("駐車場はありますか", "clinic_visitor_faq"),
    ("当日でも予約できますか", "clinic_visitor_faq"),
    ("LINEで予約できますか", "clinic_visitor_faq"),
    # EC の実務
    ("送料はいくらですか", "ec_faq"),
    ("返品できますか", "ec_faq"),
    ("領収書は出ますか", "ec_faq"),
    ("会員登録は必要ですか", "ec_account"),
    ("パスワードを忘れました", "ec_account"),
    # 遊び方
    ("ポーカーのルールを教えて", "game_rules"),
    ("ソリティアの遊び方", "game_rules"),
    ("難易度は選べますか", "game_rules"),
    # できぬこと（LLM に流すと 1B が計算を誤り、ありもしない報せを作る）
    ("計算して", "cant_do"),
    ("ニュースを教えて", "cant_do"),
    ("株価を教えて", "cant_do"),
    ("レシピを教えて", "cant_do"),
    ("1+1は", "cant_do"),
    ("恋愛相談していい？", "cant_do"),
    # 会話の潤滑油
    ("なるほど", "backchannel"),
    ("本当に？", "backchannel"),
    ("意味がわからない", "confused"),
    ("助けて", "confused"),
    ("よろしく", "greeting"),
    ("バイバイ", "bye"),
    ("今日は寒いね", "weather_chat"),
    ("お腹すいた", "food"),
    ("眠れない", "tired"),
    ("好きな色は", "hobby"),
    ("面白いね", "praise"),
    ("エンジニアになりたい", "learn"),
    # 猫の体調は animals ではなく cat_health（獣医へ寄せる）
    ("猫が吐きました", "cat_health"),
    ("猫の寿命はどのくらい", "cat_health"),
    ("うちの猫が甘えてこない", "animals"),

    # 話題を指定しない求め
    ("興味ある事教えて欲しい", "trivia"),
    ("何か面白い話をして", "trivia"),
    ("豆知識を教えて", "trivia"),
    ("もう一つ", "trivia"),          # 小噺の締めで促している言葉
    ("何か話して", "trivia"),

    # ---- lab の各デモへの導線 ----
    # （試験の無い意図を test_every_intent_is_covered が見つけて足したもの）
    ("通販サイトは", "demo_ec"),
    ("ショップはどこ", "demo_ec"),
    ("ポーカーやりたい", "demo_games"),
    ("ソリティアは", "demo_games"),
    ("このチャットの仕組みは", "demo_chat"),
    ("chatgpt なの", "demo_chat"),

    # ---- LLM を切ってある理由（llm_off）----
    # ⚠ demo_chat（作りの説明）と取り合いになる。must が同じ「llm」を含むので、
    #    分かれ目は例文の近さだけ。「何の LLM か」は demo_chat、
    #    「なぜ使わぬのか」は llm_off。両方を残して境目を守る。
    ("なぜLLMを使っていないの", "llm_off"),
    ("どうしてLLMを切ってるの", "llm_off"),
    ("LLM無効の理由は", "llm_off"),
    ("生成AIは使わないの", "llm_off"),
    ("ollamaは動いてないの", "llm_off"),
    ("何のllmを使ってる", "demo_chat"),   # 誤爆よけ: これは作りの話

    # ---- 誤爆よけ（調べ物に奪われては困るもの）----
    # 小噺の続きを促す言葉が、別の意図に取られていないか
    ("他には？", "confused"),        # 品書きを出す。trivia の続きではない
    ("何か面白いことない", "bored"),   # 遊びを勧める。小噺ではない
    ("lab とは", "lab_about"),
    ("このサイトについて教えて", "lab_about"),
    ("予約システムについて教えて", "demo_clinic"),
    ("こてつって誰", "kotetsu"),
    ("名前の意味は", "name_origin"),
]


_guard()


@pytest.mark.parametrize("text,want", CASES, ids=[c[0] for c in CASES])
def test_intent(text, want):
    """この問いは、この意図に落ちること。"""
    got, score = router.match(text)
    name = got.name if got else None
    assert name == want, f"{text!r} は {want} のはずが {name}（score={score:.2f}）"


def test_cases_are_unique():
    """同じ問いを二度書いていないこと（期待が食い違っても気づけないため）。"""
    texts = [c[0] for c in CASES]
    dup = sorted({t for t in texts if texts.count(t) > 1})
    assert not dup, f"問いが重複している: {dup}"


def test_every_intent_is_covered():
    """すべての意図に、少なくとも一つ試験があること。

    意図を足したのに一覧へ足し忘れる、が一番起きる。増えたことをここで気づかせる。
    """
    covered = {want for _, want in CASES}
    defined = {it.name for it in router.intents}
    assert not (defined - covered), (
        "試験の無い意図がある（CASES に足すこと）: " + ", ".join(sorted(defined - covered)))


@pytest.mark.parametrize("llm", [True, False])
def test_no_slot_is_left_unresolved(llm):
    """回答文の LLM 目印が、どちらの設定でも残らないこと。

    persona に無い名前を書いた（綴りを間違えた）ときに気づけるようにする。
    残ったまま出ると、利用者の画面に 〔LLM:reason〕 がそのまま出る。
    """
    for it in router.intents:
        if not it.answer:
            continue
        assert "〔LLM" not in intent_defs.apply_llm_notes(it.answer, llm), it.name
