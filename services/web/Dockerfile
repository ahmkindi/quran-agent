# web-service (prod). CGO for layeh.com/gopus (vendored libopus, static), the
# LiveKit media-sdk (hraban/opus.v2 → system libopus + libopusfile), and the
# sherpa-onnx halt-word spotter (built with -tags sherpa; bundled native .so +
# a KWS model). Runtime is debian-slim (needs those shared libs).
FROM golang:1.26-bookworm AS build
RUN apt-get update && apt-get install -y --no-install-recommends \
      libopus-dev libopusfile-dev libsoxr-dev \
      python3 python3-pip curl ca-certificates bzip2 \
    && rm -rf /var/lib/apt/lists/*
# sentencepiece is used once at build to tokenize the control words into keywords.txt.
RUN pip3 install --no-cache-dir --break-system-packages sentencepiece

# Download the English KWS model.
ARG KWS_MODEL=sherpa-onnx-kws-zipformer-gigaspeech-3.3M-2024-01-01
RUN mkdir -p /models && cd /models \
    && curl -fsSL -o kws.tar.bz2 \
       "https://github.com/k2-fsa/sherpa-onnx/releases/download/kws-models/${KWS_MODEL}.tar.bz2" \
    && tar xjf kws.tar.bz2 && mv "${KWS_MODEL}" kws && rm kws.tar.bz2

# Generate keywords.txt for the recitation control words (BPE-tokenized via the
# model's bpe.model), one line per word. @WORD sets the reported keyword string,
# which bridge's actionFor() maps to a RecitationControl action.
ARG KWS_WORDS=stop,again,next,previous
RUN cd /models/kws && python3 -c "import sentencepiece as spm,sys; \
sp=spm.SentencePieceProcessor(); sp.load('bpe.model'); \
[print(' '.join(sp.encode(w, out_type=str)) + ' @' + w) \
 for w in (x.strip().upper() for x in sys.argv[1].split(',')) if w]" \
    "${KWS_WORDS}" > keywords.txt \
    # Boost short single-token words (weakest to spot; NEXT misses without it).
    && sed -i 's/^▁NEXT @NEXT$/▁NEXT :2.5 @NEXT/' keywords.txt \
    # Aliases for "next": bare NEXT is a single weak BPE token the acoustic
    # model rarely hypothesizes; SKIP (4 tokens) and NEXT VERSE detect reliably.
    && python3 -c "import sentencepiece as spm; \
sp=spm.SentencePieceProcessor(); sp.load('bpe.model'); \
print(' '.join(sp.encode('SKIP', out_type=str)) + ' @SKIP'); \
print(' '.join(sp.encode('NEXT VERSE', out_type=str)) + ' @NEXT_VERSE')" >> keywords.txt \
    && echo "keywords.txt:" && cat keywords.txt

WORKDIR /src
COPY go.mod go.sum ./
# go.mod replaces google.golang.org/adk with this local patched copy; its
# go.mod must exist before `go mod download` resolves the graph.
COPY third_party/adk/go.mod third_party/adk/go.sum ./third_party/adk/
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -tags sherpa -trimpath -ldflags="-s -w" \
    -o /out/web ./services/web/cmd/server
# Bundle the sherpa-onnx native libs (rpath in the module cache won't exist in
# the runtime stage, so we copy them to a standard lib dir).
RUN mkdir -p /out/lib && \
    cp "$(go env GOMODCACHE)"/github.com/k2-fsa/sherpa-onnx-go-linux@*/lib/x86_64-unknown-linux-gnu/*.so* /out/lib/

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      libopus0 libopusfile0 libsoxr0 libgomp1 ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/web /usr/local/bin/web
COPY --from=build /out/lib/ /usr/local/lib/
COPY --from=build /models/kws /models/kws
RUN ldconfig
ENV LD_LIBRARY_PATH=/usr/local/lib
ENV KWS_MODEL_DIR=/models/kws
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/web"]
