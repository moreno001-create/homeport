# The local AI model doesn't answer
Signs: Ollama, model not found, can't reach 127.0.0.1:11434, local model, no chat model, AI page empty, out of memory
Applies: any

## Why
Ollama stopped, the chosen model was removed, or it doesn't fit in memory with everything else running.

## Fix
1. `ai_models_status`: the model server, the installed models and the one in use.
2. Ollama not answering: ask to `restart_service` ollama; `service_logs` ollama says why it stopped ("out of
   memory" means a smaller model is needed).
3. The chosen model is gone: choose another on the AI models page, or download it again there.
4. Ollama's settings from the installer are in /etc/systemd/system/ollama.service.d/homeport.conf (Linux): a 16K
   context and keeping the model loaded 30 minutes.
