# チャットの後段（Python）: slim + FastAPI/uvicorn/httpx
FROM python:3.14-slim
WORKDIR /app
COPY requirements.txt requirements-dev.txt ./
# 試験の道具も既定で入れる（約5MB）。動いている器に対して
#   docker compose exec -T chat-ai python -m pytest -q
# を走らせられるようにするため。配る物では、これが一番使われる確かめ方になる。
# 要らないなら --build-arg INSTALL_DEV=0 で外せる。
ARG INSTALL_DEV=1
RUN pip install --no-cache-dir -r requirements.txt \
 && if [ "$INSTALL_DEV" = "1" ]; then pip install --no-cache-dir -r requirements-dev.txt; fi
# ⚠ ファイル名を並べないこと。設置ごとの追加（labinfo.py など）を消したときに
#   ビルドが落ちる。「消しても動く」を守るため、あるものをまとめて入れる。
COPY *.py pytest.ini ./
COPY content/ ./content/
EXPOSE 8000
CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8000"]
