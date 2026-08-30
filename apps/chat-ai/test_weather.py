"""天気の地名解決の試験。

守りたいのは一つ、**名乗った通りに引けること**。
拾えなかったときの案内は「国内は全都道府県」と名乗っている。
名乗る以上、47件が引けることは機械で確かめておく。

実際、**北海道だけが引けなかった**（2026-08-30 に発覚）。
気象庁の予報区に「北海道」という単位が無く、7つの地方に分かれていて、
その名（石狩・空知・後志 等）が県名を含まないため、生成では別名が作られなかった。
鹿児島・沖縄は区域名に県名が入るので自動で拾えていた——だから気づけなかった。

⚠ 外部へは一切繋がない。地名の解決だけを見る（気象庁を試験で叩かない）。
"""

import pytest

from areas import JMA_AREAS
from weather import find_place

# 47都道府県。案内文が名乗っている範囲そのもの。
PREFECTURES = """
北海道 青森 岩手 宮城 秋田 山形 福島
茨城 栃木 群馬 埼玉 千葉 東京 神奈川
新潟 富山 石川 福井 山梨 長野 岐阜 静岡 愛知 三重
滋賀 京都 大阪 兵庫 奈良 和歌山
鳥取 島根 岡山 広島 山口 徳島 香川 愛媛 高知
福岡 佐賀 長崎 熊本 大分 宮崎 鹿児島 沖縄
""".split()


def test_all_47_prefectures_are_known():
    """47都道府県すべてが登録されていること。

    ⚠ この試験が落ちたら、案内文の「全都道府県」が嘘になっている。
      直すのは案内文ではなく areas.py（名乗った通りに引けるようにする）。
    """
    missing = [p for p in PREFECTURES if p not in JMA_AREAS]
    assert not missing, f"引けない都道府県がある: {missing}"


@pytest.mark.parametrize("pref", PREFECTURES)
def test_prefecture_is_found_in_a_sentence(pref):
    """文の中に書かれた都道府県名を拾えること。

    辞書に在ることと、文から拾えることは別。
    find_place は長い地名を優先し、1文字の地名は単独のときだけ拾う——
    その仕分けを通り抜けられるかまで見る。
    """
    name, kind = find_place(f"{pref}の天気は？")
    assert kind == "jp", f"{pref} を国内の地名として拾えていない（{name!r}, {kind!r}）"
    assert name in JMA_AREAS


def test_hokkaido_maps_to_a_real_area():
    """北海道は石狩（札幌を含む）に寄せてある。

    気象庁に「北海道」という予報区が無いため代表を選ぶしかない。
    ずれは隠さず、weather.py が「北海道（石狩地方）」と併記する。
    """
    assert JMA_AREAS["北海道"] == "016000"


def test_subdivisions_are_still_reachable():
    """道内の他所は、その名でそのまま引けること。

    代表に寄せた結果、他の地方が引けなくなっていては本末転倒。
    """
    for area in ["宗谷", "釧路", "函館", "旭川"]:
        name, kind = find_place(f"{area}の天気")
        assert kind == "jp", f"{area} を拾えていない"


def test_one_letter_place_still_needs_to_stand_alone():
    """1文字の地名（津）は単独で書かれた時だけ拾うこと。

    「〜について」「興味津々」のような並びで誤爆させない。
    """
    name, _ = find_place("津の天気は？")
    assert name == "津"

    name, kind = find_place("興味津々ですが天気は？")
    assert name != "津", "1文字の地名が語の途中で拾われている"


def test_unknown_place_is_not_guessed():
    """知らない地名は**推測しない**こと。

    分からぬまま何処かの天気を返すのが、この手のボットで最も質が悪い。
    """
    name, kind = find_place("架空の国の天気は？")
    assert name is None and kind is None
