SKIPUNZIP=0

ui_print "- ZafxyrAgent 安装器"
ui_print "- 设置权限"

set_perm_recursive $MODPATH 0 0 0755 0644
set_perm $MODPATH/bin/agentd 0 0 0755
set_perm $MODPATH/service.sh 0 0 0755

if [ -f "$MODPATH/apk/aiagent-app.apk" ]; then
  ui_print "- 安装配套 App..."
  # 本 fork 的 APK 用自己的密钥签名，和上游发布的版本签名不同。
  # 签名不一致时 pm install -r 必定失败，所以先卸掉旧包再装。
  # App 本身不存数据（聊天记录在 /data/adb/aiagent），卸载不影响会话。
  pm uninstall com.zafxyra.agent >/dev/null 2>&1
  if ! pm install "$MODPATH/apk/aiagent-app.apk" >/dev/null 2>&1; then
    pm install -r "$MODPATH/apk/aiagent-app.apk" >/dev/null 2>&1
  fi
  sleep 2
  pm grant com.zafxyra.agent android.permission.POST_NOTIFICATIONS 2>/dev/null
  appops set com.zafxyra.agent START_FOREGROUND allow 2>/dev/null
  appops set com.zafxyra.agent POST_NOTIFICATION allow 2>/dev/null
  ui_print "- App 已就绪（通知可点开聊天）"
fi

ui_print "- 完成。重启手机后，点通知栏的「ZafxyrAgent」打开界面。"
ui_print "- API Key 可以在界面里填：右上角齿轮 → 设置。"
ui_print "- 配置文件位置：/data/adb/aiagent/config.json"
