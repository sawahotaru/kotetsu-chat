"""設置ごとの追加処理（連携）の読み込みの試験。

守りたいのは一つ、**追加を消しても起動すること**。
以前は app.py が labinfo を無条件 import していたため、
内容ファイルの手引きが「消してよい」と書いているのに消すと落ちた。
"""

import pytest

from plugins import DEFAULT_HANDLERS, PluginError, load_handlers


def test_unset_loads_nothing():
    """未指定なら何も読まない＝追加を消しても起動する。"""
    assert load_handlers("") == {}
    assert load_handlers("   ,  ,") == {}


def test_missing_module_stops_with_a_reason():
    """名前を書いたのに読めないときは、黙って飛ばさず理由付きで止まること。

    「書いたのに効いていない」が一番気づきにくい失敗なので、ここは既定値で
    握り潰してはならない。
    """
    with pytest.raises(PluginError) as e:
        load_handlers("no_such_module_xyz")
    assert "no_such_module_xyz" in str(e.value)
    assert "CHAT_HANDLERS" in str(e.value)


def test_module_without_register_stops():
    """register() を持たないモジュールを指しても止まること。"""
    with pytest.raises(PluginError, match="register"):
        load_handlers("areas")   # 実在するが連携ではない


def test_labinfo_registers_expected_names():
    """同梱の見本が、intents.yaml の handler 名と噛み合っていること。

    ⚠ labinfo.py はこの lab に固有の追加。配布時は消してよい。
      無ければこの試験は飛ばす（消したら試験が落ちる、では消せない）。
    """
    labinfo = pytest.importorskip("labinfo", reason="設置ごとの追加なので無くてよい")
    provided = labinfo.register()
    assert set(provided) == {"products", "clinic"}
    assert all(callable(v) for v in provided.values())


def test_loading_the_same_name_twice_is_refused():
    """同じ処理名を二つの追加が名乗ったら止めること。

    どちらが効いているか誰にも分からなくなるため。
    """
    pytest.importorskip("labinfo", reason="設置ごとの追加なので無くてよい")
    with pytest.raises(PluginError, match="複数"):
        load_handlers("labinfo,labinfo")

def test_handler_next_to_the_content_is_found(tmp_path, monkeypatch):
    """内容ファイルの隣に置いた連携が読めること。

    ⚠ **手引きがそう書いている以上、機械で確かめる。**
      以前これが効かず、README のとおりに `my_shop.py` を置いても
      「No module named 'my_shop'」で起動しなかった（実際に踏んだ）。
    """
    import importlib
    import sys

    import plugins
    import settings

    (tmp_path / "my_shop.py").write_text(
        "async def search(text):\n"
        "    return 'ok'\n"
        "def register():\n"
        "    return {'products': search}\n",
        encoding="utf-8",
    )
    monkeypatch.setattr(settings, "CONTENT_DIR", tmp_path)
    # 探し場所を足すのは読み込みのとき。後片付けは monkeypatch に任せる。
    monkeypatch.setattr(sys, "path", list(sys.path))
    monkeypatch.delitem(sys.modules, "my_shop", raising=False)

    provided = plugins.load_handlers("my_shop")
    assert set(provided) == {"products"}
    assert callable(provided["products"])


def test_framework_wins_over_the_content_dir(tmp_path, monkeypatch):
    """内容フォルダに枠組みと同じ名前を置かれても、枠組みが勝つこと。

    負けると、内容ファイルを差し替えただけで天気の引き方が入れ替わる。
    """
    import sys

    import plugins
    import settings

    (tmp_path / "weather.py").write_text("BOGUS = True\n", encoding="utf-8")
    monkeypatch.setattr(settings, "CONTENT_DIR", tmp_path)
    monkeypatch.setattr(sys, "path", list(sys.path))

    plugins.load_handlers("")          # 探し場所は足さない（未指定のため）
    import weather
    assert hasattr(weather, "answer"), "枠組みの weather が内容フォルダのものに負けた"


# ---- 同梱の処理（weather / wikipedia）を名前で読み込む ----
#
# 以前この二つは app.py の CORE_HANDLERS に直書きされており、
#   - 追加が同じ名前を名乗ると**黙って上書き**できた
#   - 使わない設置からも外せなかった
# 読み込みを一本化したので、ここで両方を確かめる。

def test_bundled_handlers_load_by_name():
    """同梱の処理が、設置ごとの追加と同じ作法で読めること。"""
    provided = load_handlers("weather,wikipedia")
    assert set(provided) == {"weather", "wikipedia", "wikipedia_matches"}
    assert all(callable(v) for v in provided.values())


def test_default_includes_weather_and_wikipedia():
    """既定は空にしないこと。

    ⚠ 同梱の intents.yaml は案内文で「天気であれば申せる」と**直に促している**。
      既定を空にすると意図だけが自動で外れ、促しておいて答えられぬ状態になる。
      （案内文は静的なテキストなので自動では消えない）
    """
    assert set(_names(DEFAULT_HANDLERS)) == {"weather", "wikipedia"}


def _names(raw):
    return [n.strip() for n in raw.split(",") if n.strip()]


def test_bundled_handler_can_be_dropped():
    """要らない設置は減らせること。減らしても壊れないのが配布物の要件。"""
    provided = load_handlers("wikipedia")
    assert "weather" not in provided
    assert "wikipedia" in provided


def test_overriding_a_bundled_handler_is_refused(tmp_path, monkeypatch):
    """同梱の処理と同じ名前を名乗ったら**止める**こと。

    以前は黙って上書きされていた（読み込みが二系統に分かれ、重複の検査を
    通らなかったため）。差し替えたいなら先に CHAT_HANDLERS から外す——
    それが意思表示になる。
    """
    import plugins

    (tmp_path / "my_weather.py").write_text(
        "def answer(text):\n"
        "    return 'ここでは雨でござる'\n"
        "def register():\n"
        "    return {'weather': answer}\n",
        encoding="utf-8",
    )
    monkeypatch.setattr(plugins.settings, "CONTENT_DIR", tmp_path)

    with pytest.raises(PluginError) as e:
        plugins.load_handlers("weather,my_weather")
    assert "weather" in str(e.value)
    assert "CHAT_HANDLERS" in str(e.value)   # 直し方が書いてあること


def test_bundled_handler_can_be_replaced_after_dropping(tmp_path, monkeypatch):
    """外してから名乗れば、同梱の処理を差し替えられること。"""
    import plugins

    (tmp_path / "my_weather2.py").write_text(
        "def answer(text):\n"
        "    return 'ここでは雨でござる'\n"
        "def register():\n"
        "    return {'weather': answer}\n",
        encoding="utf-8",
    )
    monkeypatch.setattr(plugins.settings, "CONTENT_DIR", tmp_path)

    provided = plugins.load_handlers("wikipedia,my_weather2")
    assert provided["weather"]("東京") == "ここでは雨でござる"


def test_two_variables_are_joined(monkeypatch):
    """CHAT_HANDLERS と CHAT_SITE_HANDLERS は繋がって一本の一覧になること。

    二つに分かれているのは意味の違い（同梱か・設置ごとか）であって、
    仕組みの違いではない。
    """
    pytest.importorskip("labinfo", reason="設置ごとの追加なので無くてよい")
    monkeypatch.setenv("CHAT_HANDLERS", "wikipedia")
    monkeypatch.setenv("CHAT_SITE_HANDLERS", "labinfo")
    provided = load_handlers()
    assert "wikipedia" in provided and "products" in provided
    assert "weather" not in provided       # 既定は使わず、書いた通りになる


def test_same_name_in_both_variables_is_not_a_duplicate(monkeypatch):
    """両方に同じモジュール名を書いても重複扱いにしないこと。

    書き方の揺れで起動しなくなるのは、配る物として不親切。
    """
    monkeypatch.setenv("CHAT_HANDLERS", "wikipedia")
    monkeypatch.setenv("CHAT_SITE_HANDLERS", "wikipedia")
    provided = load_handlers()
    assert "wikipedia" in provided


def test_everything_can_be_dropped(monkeypatch):
    """全部外しても起動すること（内容ファイルの決め打ちの答えだけで動く設置）。"""
    monkeypatch.setenv("CHAT_HANDLERS", "")
    monkeypatch.setenv("CHAT_SITE_HANDLERS", "")
    assert load_handlers() == {}
