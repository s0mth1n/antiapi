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
(cd .. && CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -trimpath -ldflags="-s -w -buildid=" -o antiapi-arm64 .)
cp ../antiapi-arm64 build/lib/arm64-v8a/libantiapi.so
rm -f assets/antiapi

if [ ! -f /tmp/cloudflared-linux-arm64 ]; then
    echo "Downloading cloudflared-linux-arm64..."
    curl -sL -o /tmp/cloudflared-linux-arm64 https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-arm64
fi
cp /tmp/cloudflared-linux-arm64 build/lib/arm64-v8a/libcloudflared.so

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
