SKIPUNZIP=0

ui_print "- ZafxyrAgent AI installer"
ui_print "- Setting permissions"

set_perm_recursive $MODPATH 0 0 0755 0644
set_perm $MODPATH/bin/agentd 0 0 0755
set_perm $MODPATH/service.sh 0 0 0755

if [ -f "$MODPATH/apk/aiagent-app.apk" ]; then
  ui_print "- Installing ZafxyrAgent AI app..."
  pm install -r "$MODPATH/apk/aiagent-app.apk" 2>/dev/null
  sleep 2
  pm grant com.zafxyra.agent android.permission.POST_NOTIFICATIONS 2>/dev/null
  appops set com.zafxyra.agent START_FOREGROUND allow 2>/dev/null
  appops set com.zafxyra.agent POST_NOTIFICATION allow 2>/dev/null
  ui_print "- App ready (notif bisa ditekan)"
fi

ui_print "- Done. Reboot, then tap the 'ZafxyrAgent AI' notification."
ui_print "- Edit config.json to add your API key."