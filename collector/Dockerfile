FROM python:3.13-slim AS build
RUN apt-get update && apt-get install -y --no-install-recommends git && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY pyproject.toml ./
COPY src ./src
RUN pip wheel --no-cache-dir --wheel-dir /wheels .

FROM python:3.13-slim
COPY --from=build /wheels /wheels
# pip wheel already resolved every dependency (incl. the git-pinned one) into /wheels.
RUN pip install --no-cache-dir --no-deps /wheels/*.whl && rm -rf /wheels \
 && mkdir -p /data && chown nobody /data
# /data: provisioning state (VLANs created by this integration, last config).
VOLUME ["/data"]
USER nobody
EXPOSE 8099
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s \
  CMD ["python", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8099/healthz', timeout=3)"]
ENTRYPOINT ["netgear-collector"]
