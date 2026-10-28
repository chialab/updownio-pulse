# Built by GoReleaser (dockers_v2): binaries are pre-built in the context under $TARGETPLATFORM/.
FROM scratch
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/updownio-pulse /usr/bin/updownio-pulse
ENTRYPOINT ["/usr/bin/updownio-pulse"]
