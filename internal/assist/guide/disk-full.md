# The disk is full (or nearly)
Signs: no space left on device, disk full, 100%, apps failing, backups failing, docker pull failed
Applies: any

## Why
Docker keeps old images and build layers, apps grow (photos, media), AI models are several GB each, and logs grow.

## Fix
1. `system_health`: `df` shows what's full; `docker system df` shows how much Docker could free ("RECLAIMABLE").
2. Old Docker images are the safest to free. With shell commands on: `docker image prune -a` (removes images no app
   uses) and `docker builder prune`. Never prune volumes: they hold the apps' data.
3. Unused AI models: the AI models page lists their sizes and can remove them.
4. Then restart the apps that failed while the disk was full.
