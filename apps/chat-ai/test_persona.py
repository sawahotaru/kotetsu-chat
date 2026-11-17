"""LLM の有無で入れ替わる文面の試験。

守りたいのは、**どちらの設置でも嘘にならないこと**。
同じ頭を複数の口（Web / LINE）が共有し、口ごとに LLM の有無が違うので、
文面を一つに固定すると必ずどちらかで嘘になる。
"""

import intents as intent_defs


def test_bare_slot_switches(monkeypatch):
    """〔LLM〕 は一文の差し替え。"""
    monkeypatch.setattr(intent_defs, "LLM_NOTE_ON", "灯しておる")
    monkeypatch.setattr(intent_defs, "LLM_NOTE_OFF", "切ってある")
    assert intent_defs.apply_llm_notes("いま〔LLM〕。", True) == "いま灯しておる。"
    assert intent_defs.apply_llm_notes("いま〔LLM〕。", False) == "いま切ってある。"


def test_keyed_slot_switches(monkeypatch):
    """〔LLM:名前〕 は、答えが丸ごと入れ替わる場所のためのもの。"""
    monkeypatch.setattr(intent_defs, "_KEYED_NOTES", {"reason": ("長い理由(有)", "長い理由(無)")})
    assert intent_defs.apply_llm_notes("〔LLM:reason〕", True) == "長い理由(有)"
    assert intent_defs.apply_llm_notes("〔LLM:reason〕", False) == "長い理由(無)"


def test_keyed_slot_does_not_eat_the_bare_one(monkeypatch):
    """名前つきと素の目印が同じ文にあっても、互いを食わないこと。

    〔LLM〕 で先に置換すると 〔LLM:reason〕 の頭だけが差し替わり、
    `:reason〕` という壊れた文字列が利用者に出る。
    """
    monkeypatch.setattr(intent_defs, "LLM_NOTE_OFF", "短い")
    monkeypatch.setattr(intent_defs, "_KEYED_NOTES", {"reason": ("", "長い")})
    assert intent_defs.apply_llm_notes("〔LLM〕。\n\n〔LLM:reason〕", False) == "短い。\n\n長い"


def test_unknown_key_is_left_as_is(monkeypatch):
    """persona に無い名前は黙って消さず、そのまま残す（書き間違いに気づけるように）。"""
    monkeypatch.setattr(intent_defs, "_KEYED_NOTES", {})
    assert intent_defs.apply_llm_notes("〔LLM:typo〕", False) == "〔LLM:typo〕"


def test_legacy_label_key_still_applies(monkeypatch):
    """旧名 ollamaDown で書いた設置も、改名後の engineDown として画面に届く。"""
    monkeypatch.setattr(intent_defs, "UI", {"labels": {"ollamaDown": "旧名の文言", "stopped": "止めた"}})
    labels = intent_defs.ui_payload(True)["labels"]
    assert labels["engineDown"] == "旧名の文言"
    assert "ollamaDown" not in labels
    assert labels["stopped"] == "止めた"


def test_new_label_key_wins_over_legacy(monkeypatch):
    monkeypatch.setattr(intent_defs, "UI", {"labels": {"ollamaDown": "旧", "engineDown": "新"}})
    assert intent_defs.ui_payload(True)["labels"]["engineDown"] == "新"


def test_models_payload_names_no_product():
    """画面に返す状態の項目名に、推論基盤の製品名を出さない。"""
    import pathlib
    src = pathlib.Path(__file__).with_name("app.py").read_text(encoding="utf-8")
    assert '"ollama_reachable"' not in src
