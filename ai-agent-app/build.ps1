$ErrorActionPreference = "Stop"
$root = "D:\ZafxyraFile\AI-Agent\ai-agent-app"
$tmp  = "C:\Users\ADMINI~1\AppData\Local\Temp\opencode"
$bt   = "$tmp\sdk\android-14"
$aj   = "$tmp\sdk\android-34\android.jar"
$jd   = "$tmp\jdk\jdk-17.0.20.1+1"
$java = "$jd\bin\java.exe"; $javac = "$jd\bin\javac.exe"; $keytool = "$jd\bin\keytool.exe"

Push-Location $root

"== aapt2 compile =="
& "$bt\aapt2.exe" compile --dir res -o build\res.zip
if ($LASTEXITCODE -ne 0) { throw "aapt2 compile" }

"== aapt2 link =="
& "$bt\aapt2.exe" link -o build\app.unsigned.apk -I $aj --manifest AndroidManifest.xml --min-sdk-version 34 --target-sdk-version 34 --auto-add-overlay --java build\gen build\res.zip
if ($LASTEXITCODE -ne 0) { throw "aapt2 link" }

"== javac =="
$srcs = (Get-ChildItem -Recurse java\*.java | ForEach-Object { $_.FullName }) + (Get-ChildItem -Recurse build\gen\*.java | ForEach-Object { $_.FullName })
if (Test-Path build\classes) { Remove-Item -Recurse -Force build\classes }
& $javac -source 8 -target 8 -encoding utf-8 -classpath $aj -d build\classes $srcs
if ($LASTEXITCODE -ne 0) { throw "javac" }

"== d8 =="
New-Item -ItemType Directory -Force build\dex | Out-Null
& $java -cp "$bt\lib\d8.jar" com.android.tools.r8.D8 --release --lib $aj --min-api 34 --output build\dex build\classes\com\zafxyra\agent\*.class
if ($LASTEXITCODE -ne 0) { throw "d8" }

"== add classes.dex ke apk =="
Add-Type -AssemblyName System.IO.Compression
Add-Type -AssemblyName System.IO.Compression.FileSystem
$fs = [System.IO.File]::Open("$root\build\app.unsigned.apk", [System.IO.FileMode]::Open)
$ar = New-Object System.IO.Compression.ZipArchive($fs, [System.IO.Compression.ZipArchiveMode]::Update)
$e = $ar.CreateEntry("classes.dex", [System.IO.Compression.CompressionLevel]::Optimal)
$s = $e.Open(); $b = [System.IO.File]::ReadAllBytes("$root\build\dex\classes.dex"); $s.Write($b, 0, $b.Length); $s.Close()
$ar.Dispose(); $fs.Close()

"== zipalign =="
& "$bt\zipalign.exe" -f 4 build\app.unsigned.apk build\app.aligned.apk
if ($LASTEXITCODE -ne 0) { throw "zipalign" }

"== keystore + sign =="
if (-not (Test-Path "$root\build\zafxyr.jks")) {
    & $keytool -genkeypair -keystore "$root\build\zafxyr.jks" -storepass android -alias zafxyr -keypass android -keyalg RSA -keysize 2048 -validity 10000 -dname "CN=ZafxyrAgent AI"
    if ($LASTEXITCODE -ne 0) { throw "keytool" }
}
$env:JAVA_HOME = $jd
$env:PATH = "$jd\bin;" + $env:PATH
& "$bt\apksigner.bat" sign --ks "$root\build\zafxyr.jks" --ks-pass pass:android --key-pass pass:android --out "$root\build\aiagent-app.apk" "$root\build\app.aligned.apk"
if ($LASTEXITCODE -ne 0) { throw "apksigner" }

"== verify =="
& "$bt\apksigner.bat" verify "$root\build\aiagent-app.apk"
if ($LASTEXITCODE -ne 0) { throw "apksigner verify" }

"APK OK: $root\build\aiagent-app.apk (" + [math]::Round((Get-Item "$root\build\aiagent-app.apk").Length/1KB,1) + " KB)"
Pop-Location