FROM gcr.io/distroless/static

# Copy the binary we built from the builder image
COPY rgw-exporter /usr/local/bin/rgw-exporter

ENTRYPOINT [ "/usr/local/bin/rgw-exporter" ]
