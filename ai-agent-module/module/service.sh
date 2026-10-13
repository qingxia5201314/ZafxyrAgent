#!/system/bin/sh
MODDIR=${0%/*}
DATA=/data/adb/aiagent
mkdir -p "$DATA"

until [ "$(getprop sys.boot_completed)" = "1" ]; do sleep 2; done
sleep 5

# Pastikan app 'ZafxyrAgent AI' boleh posting notif (bisa ke-reset setelah reinstall)
pm grant com.zafxyra.agent android.permission.POST_NOTIFICATIONS 2>/dev/null
appops set com.zafxyra.agent START_FOREGROUND allow 2>/dev/null
appops set com.zafxyra.agent POST_NOTIFICATION allow 2>/dev/null

pkill -f "$MODDIR/bin/agentd" 2>/dev/null

"$MODDIR/bin/agentd" -moddir "$MODDIR" -home "$DATA" >>"$DATA/agentd.log" 2>&1 &

# Backup: pastikan notif app 'ZafxyrAgent AI' (com.zafxyra.agent) muncul.
# Pake 'am start ... boot' (bukan startservice) karena startservice
# diblok sistem saat app di background, sedangkan activity boleh.
sleep 2
am start -n com.zafxyra.agent/.MainActivity --es url http://127.0.0.1:8787 --ez boot true >/dev/null 2>&1