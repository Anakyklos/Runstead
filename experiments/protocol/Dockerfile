FROM alpine:3.22

RUN apk add --no-cache bash ca-certificates curl jq

WORKDIR /workspace
COPY experiments/protocol /workspace/experiments/protocol

ENTRYPOINT ["bash", "experiments/protocol/run.sh"]
CMD ["--offline"]
