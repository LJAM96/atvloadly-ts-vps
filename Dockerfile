FROM node:20-bullseye AS frontend-builder
WORKDIR /src
COPY ./locales ./locales
WORKDIR /src/web/static
COPY ./web/static/package.json ./package.json
COPY ./web/static/package-lock.json ./package-lock.json
RUN npm ci
COPY ./web/static ./
RUN npm run build

FROM golang:1.24-bullseye AS go-builder
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY ./go.mod ./go.mod
COPY ./go.sum ./go.sum
RUN go mod download
COPY ./cmd ./cmd
COPY ./doc ./doc
COPY ./internal ./internal
COPY ./locales ./locales
COPY ./main.go ./main.go
COPY ./web ./web
COPY --from=frontend-builder /src/web/static/dist ./web/static/dist
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /out/atvloadly ./main.go

FROM rust:bookworm AS appletvpair-builder
WORKDIR /build
COPY ./tools/appletvpair/Cargo.toml ./Cargo.toml
COPY ./tools/appletvpair/src ./src
RUN cargo build --release

FROM ubuntu:22.04 AS pmd3-builder
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get -y install \
    python3 python3-pip python3-venv build-essential libssl-dev libffi-dev
RUN python3 -m venv /opt/pmd3
RUN /opt/pmd3/bin/pip install --upgrade pip setuptools wheel
RUN /opt/pmd3/bin/pip install pymobiledevice3==9.5.1

FROM ubuntu:22.04
ARG APP_NAME
ARG VERSION
ARG BUILDDATE
ARG COMMIT
ARG TARGETPLATFORM
ARG TARGETOS
ARG TARGETARCH
RUN echo "I'm building for $TARGETPLATFORM"

# Install dependencies
RUN apt-get update && apt-get -y install \
    wget unzip libavahi-compat-libdnssd-dev curl python3 python3-venv

RUN case ${TARGETARCH} in \
         "amd64")  PKG_ARCH=x86_64  ;; \
         "arm64")  PKG_ARCH=aarch64  ;; \
    esac \
    && cd /tmp \
    && wget https://github.com/bitxeno/usbmuxd2/releases/download/v0.0.4/usbmuxd2-ubuntu-${PKG_ARCH}.tar.gz \
    && tar zxf usbmuxd2-ubuntu-${PKG_ARCH}.tar.gz \
    && dpkg -i ./libusb_1.0.26-1_${PKG_ARCH}.deb \
    && dpkg -i ./libgeneral_1.0.0-1_${PKG_ARCH}.deb \
    && dpkg -i ./libplist_2.6.0-1_${PKG_ARCH}.deb \
    && dpkg -i ./libtatsu_1.0.3-1_${PKG_ARCH}.deb \
    && dpkg -i ./libimobiledevice-glue_1.3.0-1_${PKG_ARCH}.deb \
    && dpkg -i ./libusbmuxd_2.3.0-1_${PKG_ARCH}.deb \
    && dpkg -i ./libimobiledevice_1.3.1-1_${PKG_ARCH}.deb \
    && dpkg -i ./usbmuxd2_1.0.0-1_${PKG_ARCH}.deb

# Install PlumeImpactor
RUN case ${TARGETARCH} in \
         "amd64")  PKG_ARCH=x86_64  ;; \
         "arm64")  PKG_ARCH=aarch64  ;; \
    esac \
    && cd /tmp \
    && wget https://github.com/bitxeno/PlumeImpactor/releases/download/v2.0.0-patch.4/plumesign-linux-${PKG_ARCH}.tar.gz \
    && tar zxf plumesign-linux-${PKG_ARCH}.tar.gz \
    && mv plumesign-linux-${PKG_ARCH} /usr/bin/plumesign \
    && chmod +x /usr/bin/plumesign

# Download anisette dependency library
RUN case ${TARGETARCH} in \
         "amd64")  PKG_ARCH=x86_64  ;; \
         "arm64")  PKG_ARCH=arm64-v8a  ;; \
    esac \
    && mkdir -p /keep \
    && cd /keep \
    && wget https://apps.mzstatic.com/content/android-apple-music-apk/applemusic.apk \
    && unzip applemusic.apk lib/${PKG_ARCH}/libstoreservicescore.so lib/${PKG_ARCH}/libCoreADI.so \
    && rm applemusic.apk

# Install tzdata to support timezone updates.
RUN DEBIAN_FRONTEND=noninteractive apt-get -y install tzdata

# Clear apt cache and temporary data to reduce image size.
RUN apt-get clean
RUN cd /tmp && rm -rf ./*.deb && rm -rf ./*.tar.gz && rm -rf ./*.zip && rm -rf ./*.apk

# The add command will automatically decompress the file.
RUN mkdir -p /keep
COPY ./doc/config.yaml.example /keep/config.yaml
COPY --from=go-builder /out/atvloadly /usr/bin/${APP_NAME}
COPY --from=appletvpair-builder /build/target/release/appletvpair /usr/bin/appletvpair
COPY --from=pmd3-builder /opt/pmd3 /opt/pmd3
COPY ./tools/appletvremote/appletvremote.py /usr/bin/appletvremote
RUN chmod +x /usr/bin/${APP_NAME}
RUN chmod +x /usr/bin/appletvpair
RUN chmod +x /usr/bin/appletvremote

# The lockdown records have been moved to /data.
RUN rm -rf /var/lib/lockdown && mkdir -p /data/lockdown && ln -s /data/lockdown /var/lib/lockdown



# Generate startup script
COPY ./doc/scripts/usbmuxd /etc/init.d/usbmuxd
RUN chmod +x /etc/init.d/usbmuxd
RUN printf '#!/bin/sh \n\n\

mkdir -p $HOME \n\
mkdir -p /data/lockdown \n\
mkdir -p /data/pymobiledevice3 \n\
mkdir -p /data/PlumeImpactor \n\
mkdir -p $HOME/.config \n\
[ ! -e "$HOME/.config/PlumeImpactor" ] && ln -s /data/PlumeImpactor $HOME/.config/PlumeImpactor \n\
[ ! -e "$HOME/.pymobiledevice3" ] && ln -s /data/pymobiledevice3 $HOME/.pymobiledevice3 \n\

if [ -d "/keep/lib" ]; then  \n\
    rm -rf /data/PlumeImpactor/lib \n\
    cp -rf /keep/lib /data/PlumeImpactor/lib \n\
    rm -rf /keep/lib \n\
fi  \n\

if [ ! -f "/data/config.yaml" ]; then  \n\
    cp /keep/config.yaml /data/config.yaml \n\
fi  \n\

/etc/init.d/usbmuxd start \n\

/usr/bin/%s server -p ${SERVICE_PORT:-80} -c /data/config.yaml  \n\
\n\
' ${APP_NAME} >> /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]

EXPOSE 80
VOLUME /data
