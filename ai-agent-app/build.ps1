# 构建 aiagent-app.apk
# 需要一个 JDK 17 和 Android SDK build-tools + platform-34。
# 用法: pwsh -File build.ps1
$ErrorActionPreference = "Stop"

$root = $PSScriptRoot
$bt   = "E:\tools\android-sdk\build-tools\34.0.0"
$aj   = "E:\tools\android-sdk\platforms\android-34\android.jar"
$jd   = "C:\Program Files\Java\jdk-17"
$java = "$jd\bin\java.exe"; $javac = "$jd\bin\javac.exe"; $keytool = "$jd\bin\keytool.exe"

foreach ($p in @($bt, $aj, $java, $javac, $keytool)) {
  if (-not (Test-Path $p)) { throw "缺少依赖: $p" }
}

Push-Location $root
try {
  if (Test-Path build) { Remove-Item -Recurse -Force build }
  New-Item -ItemType Directory -Force build | Out-Null

  "== aapt2 compile =="
  & "$bt\aapt2.exe" compile --dir res -o build\res.zip
  if ($LASTEXITCODE -ne 0) { throw "aapt2 compile failed" }

  "== aapt2 link =="
  & "$bt\aapt2.exe" link -o build\app.unsigned.apk -I $aj `
      --manifest AndroidManifest.xml --min-sdk-version 34 --target-sdk-version 34 `
      --auto-add-overlay --java build\gen build\res.zip
  if ($LASTEXITCODE -ne 0) { throw "aapt2 link failed" }

  "== javac =="
  $srcs = (Get-ChildItem -Recurse java\*.java | ForEach-Object { $_.FullName }) +
          (Get-ChildItem -Recurse build\gen\*.java | ForEach-Object { $_.FullName })
  & $javac -source 8 -target 8 -encoding utf-8 -classpath $aj -d build\classes $srcs 2>&1 |
    Where-Object { $_ -notmatch "bootstrap class path|source value 8|target value 8|deprecat" }
  if ($LASTEXITCODE -ne 0) { throw "javac failed" }

  "== d8 =="
  New-Item -ItemType Directory -Force build\dex | Out-Null
  & $java -cp "$bt\lib\d8.jar" com.android.tools.r8.D8 --release --lib $aj --min-api 34 `
      --output build\dex (Get-ChildItem -Recurse build\classes\com\zafxyra\agent\*.class |
        ForEach-Object { $_.FullName })
  if ($LASTEXITCODE -ne 0) { throw "d8 failed" }

  "== 把 classes.dex 塞进 apk =="
  Add-Type -AssemblyName System.IO.Compression
  Add-Type -AssemblyName System.IO.Compression.FileSystem
  $fs = [System.IO.File]::Open("$root\build\app.unsigned.apk", [System.IO.FileMode]::Open)
  $ar = New-Object System.IO.Compression.ZipArchive($fs, [System.IO.Compression.ZipArchiveMode]::Update)
  $e = $ar.CreateEntry("classes.dex", [System.IO.Compression.CompressionLevel]::Optimal)
  $s = $e.Open()
  $b = [System.IO.File]::ReadAllBytes("$root\build\dex\classes.dex")
  $s.Write($b, 0, $b.Length); $s.Close()
  $ar.Dispose(); $fs.Close()

  "== zipalign =="
  & "$bt\zipalign.exe" -f 4 build\app.unsigned.apk build\app.aligned.apk
  if ($LASTEXITCODE -ne 0) { throw "zipalign failed" }

  "== 签名 =="
  if (-not (Test-Path "$root\build\zafxyr.jks")) {
    & $keytool -genkeypair -keystore "$root\build\zafxyr.jks" -storepass android `
        -alias zafxyr -keypass android -keyalg RSA -keysize 2048 -validity 10000 `
        -dname "CN=ZafxyrAgent"
    if ($LASTEXITCODE -ne 0) { throw "keytool failed" }
  }
  $env:JAVA_HOME = $jd
  $env:PATH = "$jd\bin;" + $env:PATH
  & "$bt\apksigner.bat" sign --ks "$root\build\zafxyr.jks" --ks-pass pass:android `
      --key-pass pass:android --out "$root\build\aiagent-app.apk" "$root\build\app.aligned.apk"
  if ($LASTEXITCODE -ne 0) { throw "apksigner sign failed" }

  "== 校验 =="
  & "$bt\apksigner.bat" verify "$root\build\aiagent-app.apk"
  if ($LASTEXITCODE -ne 0) { throw "apksigner verify failed" }

  $kb = [math]::Round((Get-Item "$root\build\aiagent-app.apk").Length / 1KB, 1)
  "APK OK: build\aiagent-app.apk ($kb KB)"
} finally {
  Pop-Location
}
