# The server is slow, or out of memory
Signs: slow, high load, swap, out of memory, OOM, killed, memory, CPU 100%
Applies: any

## Why
Too much running at once for the machine: big apps (Nextcloud, Immich's machine learning, Jellyfin transcoding) and a
local AI model share the same memory and processor.

## Fix
1. `system_health` and `services_status`: which app or service uses the memory and processor.
2. A local model holds its memory for 30 minutes after each use; a smaller model frees a lot.
3. Apps nobody uses can be stopped (Apps page) or removed.
4. "Killed" or "OOM" in an app's log: it ran out of memory; it needs more memory or fewer neighbours.
