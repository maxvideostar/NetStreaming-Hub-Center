@echo off
chcp 65001 >nul
REM nTs-Suite – instalare/actualizare pe Windows ca serviciu (panou http://localhost:8090)
net session >nul 2>&1
if errorlevel 1 (
  echo Pornesc ca Administrator...
  powershell -NoProfile -Command "Start-Process -FilePath '%~f0' -Verb RunAs"
  exit /b
)
set "DEST=C:\nTs-Suite"
set "SRC=%~dp0nts-suite-windows-amd64.exe"
if not exist "%SRC%" set "SRC=%~dp0nts-suite.exe"
if not exist "%SRC%" (echo Nu gasesc nts-suite-windows-amd64.exe langa acest fisier & pause & exit /b 1)

echo.
echo == Opresc vechile servicii separate (daca exista), ca sa elibereze porturile ==
for %%S in (ProxyHub rtmp-media-server VideoEncoder Rec-Martori RecMartori StreamHub nTs-relay) do (
  sc query "%%S" >nul 2>&1 && (sc stop "%%S" >nul 2>&1 & sc config "%%S" start= disabled >nul 2>&1 & echo   %%S dezactivat)
)
schtasks /Delete /TN "VideoEncoder" /F >nul 2>&1
schtasks /Delete /TN "StreamHub" /F >nul 2>&1

if not exist "%DEST%" mkdir "%DEST%"
sc query nTs-Suite >nul 2>&1 && (sc stop nTs-Suite >nul 2>&1 & timeout /t 4 >nul)
copy /Y "%SRC%" "%DEST%\nts-suite.exe" >nul || (echo Copiere esuata & pause & exit /b 1)

sc query nTs-Suite >nul 2>&1
if errorlevel 1 (
  "%DEST%\nts-suite.exe" -data "%DEST%\data" install
) else (
  "%DEST%\nts-suite.exe" -data "%DEST%\data" firewall
  sc start nTs-Suite >nul
  echo Actualizat si repornit.
)
echo.
echo Panou:  http://localhost:8090   (prima autentificare: admin / admin)
echo Date:   %DEST%\data
echo ffmpeg: pune ffmpeg.exe + ffprobe.exe in %DEST%\data\bin  (sau in PATH)
start "" http://localhost:8090
pause
