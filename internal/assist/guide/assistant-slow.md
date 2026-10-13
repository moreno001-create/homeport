# The assistant is slow, or seems stuck
Signs: assistant slow, stuck, stalls, thinking forever, reading the conversation, local model, Ollama slow
Applies: any

## Why
A local model on a processor (no graphics card it can use) first reads the whole conversation, Homeport's
instructions and tools included: on an older processor that takes a minute or two for a new conversation, then it
writes a few words a second. The chat shows "Reading the conversation… 1:20" while it does.

## Fix
1. `ai_models_status`: the model in use, how fast it measured, and whether Ollama uses the graphics card.
2. A smaller model is faster (the AI models page marks the best fit for this server); a supported graphics card
   (NVIDIA, or a recent AMD) makes it many times faster.
3. Shorter conversations read faster: start a new chat for a new topic.
4. If Ollama itself stopped answering (`services_status` shows it failed), ask to `restart_service` ollama.
