"""設置ごとの追加処理（連携）の読み込みの試験。

守りたいのは一つ、**追加を消しても起動すること**。
以前は app.py が labinfo を無条件 import していたため、
内容ファイルの手引きが「消してよい」と書いているのに消すと落ちた。
"""

import pytest

from plugins import PluginError, load_site_handlers


def test_unset_loads_nothing():
    """未指定なら何も読まない＝追加を消しても起動する。"""
    assert load_site_handlers("") == {}
    assert load_site_handlers("   ,  ,") == {}


def test_missing_module_stops_with_a_reason():
    """名前を書いたのに読めないときは、黙って飛ばさず理由付きで止まること。

    「書いたのに効いていない」が一番気づきにくい失敗なので、ここは既定値で
    握り潰してはならない。
    """
    with pytest.raises(PluginError) as e:
        load_site_handlers("no_such_module_xyz")
    assert "no_such_module_xyz" in str(e.value)
    assert "CHAT_SITE_HANDLERS" in str(e.value)


def test_module_without_register_stops():
    """register() を持たないモジュールを指しても止まること。"""
    with pytest.raises(PluginError, match="register"):
        load_site_handlers("areas")   # 実在するが連携ではない


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
        load_site_handlers("labinfo,labinfo")

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

    provided = plugins.load_site_handlers("my_shop")
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

    plugins.load_site_handlers("")          # 探し場所は足さない（未指定のため）
    import weather
    assert hasattr(weather, "answer"), "枠組みの weather が内容フォルダのものに負けた"
