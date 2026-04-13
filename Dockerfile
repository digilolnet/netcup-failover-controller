FROM gcr.io/distroless/static:nonroot
ARG TARGETOS
ARG TARGETARCH
COPY bin/manager-${TARGETOS}-${TARGETARCH} /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
