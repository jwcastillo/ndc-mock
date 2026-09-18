# The image carries the engine and its configuration, never a reference
# response: that is provider data. Mount it at /stubs.
FROM golang:1.25 AS build
WORKDIR /src
COPY edge/ .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /ndc-edge-mock .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /ndc-edge-mock /ndc-edge-mock
COPY routes.json versions.json airlines.json /etc/ndc-mock/
COPY translations/ /etc/ndc-mock/translations/
EXPOSE 8090
ENTRYPOINT ["/ndc-edge-mock", "-stubs", "/stubs", \
  "-config", "/etc/ndc-mock/routes.json", "-versions", "/etc/ndc-mock/versions.json", \
  "-airlines", "/etc/ndc-mock/airlines.json", "-translations", "/etc/ndc-mock/translations"]
