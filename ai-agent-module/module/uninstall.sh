#!/system/bin/sh
pkill -f "/data/adb/modules/aiagent/bin/agentd" 2>/dev/null
rm -rf /data/adb/aiagent
