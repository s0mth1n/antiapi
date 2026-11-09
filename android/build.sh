#!/usr/bin/env bash
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$DIR"

SDK="/opt/android-sdk"
BUILD_TOOLS="$SDK/build-tools/35.0.0"
PLATFORM="$SDK/platforms/android-35/android.jar"
AAPT2="$BUILD_TOOLS/aapt2"
D8="$BUILD_TOOLS/d8"
ZIPALIGN="$BUILD_TOOLS/zipalign"
APKSIGNER="$BUILD_TOOLS/apksigner"

echo "==> Preparing build directory..."
rm -rf build
mkdir -p build/obj build/lib/arm64-v8a assets

echo "==> Preparing native arm64 binaries (ponytail optimized)..."
(cd "$DIR" && CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -trimpath -ldflags="-s -w -buildid=" -o build/lib/arm64-v8a/libantiapi.so ./cmd/main.go)
rm -f assets/antiapi

CF_BIN="$HOME/.cache/antiapi/libcloudflared.so"
if [ ! -f "$CF_BIN" ]; then
    echo "==> Building Android-compatible cloudflared with DNS fallback..."
    CF_SRC="/tmp/cf-src"
    if [ ! -d "$CF_SRC" ]; then
        git clone --depth 1 https://github.com/cloudflare/cloudflared "$CF_SRC"
    fi
    if ! grep -q "net.DefaultResolver" "$CF_SRC/cmd/cloudflared/main.go"; then
        sed -i 's|"github.com/getsentry/sentry-go"|"github.com/getsentry/sentry-go"\n\t"net"\n\t"context"|' "$CF_SRC/cmd/cloudflared/main.go"
        sed -i '/func main()/i func init() { if _, err := os.Stat("/etc/resolv.conf"); err != nil { net.DefaultResolver = \&net.Resolver{ PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) { d := net.Dialer{Timeout: 3 * time.Second}; conn, err := d.DialContext(ctx, "udp", "1.1.1.1:53"); if err != nil { return d.DialContext(ctx, "udp", "8.8.8.8:53"); }; return conn, nil; } }; }; if os.Getenv("SSL_CERT_DIR") == "" { if _, err := os.Stat("/system/etc/security/cacerts"); err == nil { _ = os.Setenv("SSL_CERT_DIR", "/system/etc/security/cacerts"); }; }; }\n' "$CF_SRC/cmd/cloudflared/main.go"
    fi
    mkdir -p "$(dirname "$CF_BIN")"
    (cd "$CF_SRC" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w -buildid=" -o "$CF_BIN" ./cmd/cloudflared)
fi
cp "$CF_BIN" build/lib/arm64-v8a/libcloudflared.so

STRIP="$(find "$SDK/ndk" -name "llvm-strip" 2>/dev/null | head -n 1 || true)"
if [ -n "$STRIP" ]; then
    echo "==> Stripping native binaries with llvm-strip..."
    "$STRIP" --strip-all build/lib/arm64-v8a/*.so
fi

echo "==> Compiling resources..."
"$AAPT2" compile --dir res -o build/res.zip

echo "==> Linking APK..."
"$AAPT2" link -I "$PLATFORM" \
    -R build/res.zip \
    --auto-add-overlay \
    --manifest AndroidManifest.xml \
    -o build/antiapi.unaligned.apk \
    --java src/ \
    -A assets

echo "==> Compiling Java sources..."
javac -cp "$PLATFORM" -d build/obj src/io/antiapi/android/*.java

echo "==> Generating classes.dex with d8..."
"$D8" --lib "$PLATFORM" --output build/ build/obj/io/antiapi/android/*.class

echo "==> Packaging DEX and native libraries into APK..."
(
    cd build
    zip -9 -u antiapi.unaligned.apk classes.dex
    zip -9 -u -r antiapi.unaligned.apk lib/
)

echo "==> Aligning APK..."
"$ZIPALIGN" -f -p 4 build/antiapi.unaligned.apk build/antiapi.aligned.apk

echo "==> Signing APK..."
KEYSTORE="$DIR/keystore/debug.keystore"
if [ ! -f "$KEYSTORE" ]; then
    mkdir -p "$DIR/keystore"
    keytool -genkeypair -keystore "$KEYSTORE" -storepass android -keypass android \
        -alias androiddebugkey -dname "CN=AntiAPI, OU=Dev, O=AntiAPI, C=US" \
        -keyalg RSA -keysize 2048 -validity 10000
fi

"$APKSIGNER" sign --ks "$KEYSTORE" --ks-pass pass:android --key-pass pass:android \
    --out build/antiapi.apk build/antiapi.aligned.apk

echo "==> Build successful: $(pwd)/build/antiapi.apk ($(ls -lh build/antiapi.apk | awk '{print $5}'))"
