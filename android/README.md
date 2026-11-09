# AntiAPI Android Application

Native Android application and background service packaging AntiAPI as an on-device API gateway.

## Structure
- `cmd/main.go`: Dedicated pure-server Go binary optimized for Android (DNS overrides, zero terminal/TUI bloat, low-memory footprint).
- `src/io/antiapi/android/`: Native Java application, Foreground Service, battery wakelock management, and Jetpack UI.
- `res/`: Modern dark-mode layout, progress bars, status indicators, and account management cards.
- `build.sh`: Gradle-free pure Android SDK/NDK toolchain script compiling native ARM64 libraries, AAPT2 resources, and signed APK.

## Building the APK

Requirements:
- Android SDK (`build-tools/35.0.0`, `platforms/android-35`)
- Go toolchain (`go`)
- `zip`, `javac`

Run the build script:
```bash
cd android
./build.sh
```
The output APK will be generated at `android/build/antiapi.apk`.

## Installing to Device / Emulator

```bash
adb install -r android/build/antiapi.apk
```

