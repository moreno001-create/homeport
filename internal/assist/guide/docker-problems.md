# Docker isn't running, or apps can't download images
Signs: docker, Cannot connect to the Docker daemon, pull access denied, toomanyrequests, image, registry, OrbStack, Colima
Applies: any

## Why
Every app runs in Docker. If Docker stopped, every app is down. Image downloads fail when the registry limits this
address (Docker Hub's pull limits), the network is down, or the disk is full. On a Mac, the Docker runtime (OrbStack,
Colima, Docker Desktop) has to be open.

## Fix
1. `services_status`: Docker's state (Linux). On a Mac, ask the admin to open their Docker app.
2. `service_logs` docker for the reason.
3. Ask to `restart_service` docker (every app stops and starts again).
4. "toomanyrequests": wait an hour, or sign Docker in to a Docker Hub account.
