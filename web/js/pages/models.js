/**
 * 模型路由管理页面 - 组合式函数
 * 支持同步、启停、添加/编辑与自定义字段保护。
 */

import { validateRequired } from "../api.js?v=20260619a";
const { ref, reactive, computed, watch } = Vue;

/**
 * 本地兜底 Go 模型列表；实际可用模型以后台同步后的 /admin/models 为准。
 */
const GO_MODELS = [
  { id: "glm-5.2", name: "GLM-5.2", protocol: "chat", upstream: "go" },
  { id: "glm-5.1", name: "GLM-5.1", protocol: "chat", upstream: "go" },
  { id: "glm-5", name: "GLM-5", protocol: "chat", upstream: "go" },
  { id: "kimi-k2.7-code", name: "Kimi K2.7 Code", protocol: "chat", upstream: "go" },
  { id: "kimi-k2.6", name: "Kimi K2.6", protocol: "chat", upstream: "go" },
  { id: "kimi-k2.5", name: "Kimi K2.5", protocol: "chat", upstream: "go" },
  { id: "deepseek-v4-pro", name: "DeepSeek V4 Pro", protocol: "chat", upstream: "go" },
  { id: "deepseek-v4-flash", name: "DeepSeek V4 Flash", protocol: "chat", upstream: "go" },
  { id: "mimo-v2.5", name: "MiMo-V2.5", protocol: "chat", upstream: "go" },
  { id: "mimo-v2.5-pro", name: "MiMo-V2.5-Pro", protocol: "chat", upstream: "go" },
  { id: "mimo-v2-pro", name: "MiMo V2 Pro", protocol: "chat", upstream: "go" },
  { id: "mimo-v2-omni", name: "MiMo V2 Omni", protocol: "chat", upstream: "go" },
  { id: "hy3-preview", name: "HY3 Preview", protocol: "chat", upstream: "go" },
  { id: "minimax-m3", name: "MiniMax M3", protocol: "messages", upstream: "go" },
  { id: "minimax-m2.7", name: "MiniMax M2.7", protocol: "messages", upstream: "go" },
  { id: "minimax-m2.5", name: "MiniMax M2.5", protocol: "messages", upstream: "go" },
  { id: "qwen3.7-max", name: "Qwen3.7 Max", protocol: "messages", upstream: "go" },
  { id: "qwen3.7-plus", name: "Qwen3.7 Plus", protocol: "messages", upstream: "go" },
  { id: "qwen3.6-plus", name: "Qwen3.6 Plus", protocol: "messages", upstream: "go" },
  { id: "qwen3.5-plus", name: "Qwen3.5 Plus", protocol: "messages", upstream: "go" },
];

/** Ollama Cloud seed models — gateway-facing ids mapped to real upstream model names. */
const OLLAMA_MODELS = [
  { id: "gpt-oss:120b", name: "GPT-OSS 120B", protocol: "chat", upstream: "ollama" },
  { id: "gpt-oss:20b", name: "GPT-OSS 20B", protocol: "chat", upstream: "ollama" },
  { id: "qwen3.5:397b", name: "Qwen3.5 397B", protocol: "chat", upstream: "ollama" },
  { id: "gemma4:31b", name: "Gemma4 31B", protocol: "chat", upstream: "ollama" },
  { id: "mistral-large-3:675b", name: "Mistral Large 3 675B", protocol: "chat", upstream: "ollama" },
  { id: "nemotron-3-ultra", name: "Nemotron 3 Ultra", protocol: "chat", upstream: "ollama" },
  { id: "nemotron-3-super", name: "Nemotron 3 Super", protocol: "chat", upstream: "ollama" },
  { id: "nemotron-3-nano:30b", name: "Nemotron 3 Nano 30B", protocol: "chat", upstream: "ollama" },
];

export function useModels(api, showToast, t, showConfirm) {
  const models = ref([]);
  const showModal = ref(false);
  const editingId = ref("");
  const syncing = ref(false);

  const newModel = reactive({
    id: "",
    name: "",
    upstream: "go",
    protocol: "chat",
    real_model: "",
    context_len: 0,
    status: 1,
    priority: 0,
    tags_text: "",
    prompt_price: "",
    completion_price: "",
    cache_read_price: "",
    cache_write_price: "",
  });

  /** 当前上游可选的模型列表：内置兜底 + 已同步数据。 */
  const availableModels = computed(() => {
    const byId = new Map([...GO_MODELS, ...OLLAMA_MODELS].map((m) => [m.id, m]));
    for (const m of models.value || []) {
      if (!byId.has(m.real_model || m.id)) {
        byId.set(m.real_model || m.id, {
          id: m.real_model || m.id,
          name: m.name || m.id,
          protocol: m.protocol || "chat",
          upstream: m.upstream || "go",
        });
      }
    }
    return Array.from(byId.values()).sort((a, b) => a.id.localeCompare(b.id));
  });

  /** 当 Go 模型变化时，自动同步模型 ID、名称与协议。 */
  watch(
    () => newModel.real_model,
    (val) => {
      if (editingId.value) return;
      const found = availableModels.value.find((m) => m.id === val);
      if (!found) return;
      newModel.id = found.id;
      newModel.name = found.name;
      newModel.protocol = found.protocol;
      newModel.upstream = found.upstream || "go";
    }
  );

  /** 当名称变化时，自动填充模型 ID */
  watch(
    () => newModel.name,
    (val) => {
      if (editingId.value) return;
      if (val && val.trim() && !newModel.real_model) {
        newModel.id = toSlug(val);
      }
    }
  );

  /** 辅助：将名称转为 ID 格式 */
  function toSlug(str) {
    return str
      .toLowerCase()
      .replace(/[^a-z0-9\u4e00-\u9fa5]+/g, "-")
      .replace(/^-+|-+$/g, "");
  }

  function resetForm() {
    editingId.value = "";
    newModel.id = "";
    newModel.name = "";
    newModel.upstream = "go";
    newModel.protocol = "chat";
    newModel.real_model = "";
    newModel.context_len = 0;
    newModel.status = 1;
    newModel.priority = 0;
    newModel.tags_text = "";
    newModel.prompt_price = "";
    newModel.completion_price = "";
    newModel.cache_read_price = "";
    newModel.cache_write_price = "";
  }

  /** 打开新增或编辑模态框 */
  function openModal(model) {
    resetForm();
    if (model && model.id) {
      editingId.value = model.id;
      newModel.id = model.id;
      newModel.name = model.name || model.id;
      newModel.upstream = model.upstream || "go";
      newModel.protocol = model.protocol || "chat";
      newModel.real_model = model.real_model || model.id;
      newModel.context_len = model.context_len || model.context_length || 0;
      newModel.status = model.status === 0 ? 0 : 1;
      newModel.priority = model.priority || 0;
      newModel.tags_text = (model.tags || []).join(", ");
      newModel.prompt_price = model.pricing?.prompt || "";
      newModel.completion_price = model.pricing?.completion || "";
      newModel.cache_read_price = model.pricing?.input_cache_read || "";
      newModel.cache_write_price = model.pricing?.input_cache_write || "";
    }
    showModal.value = true;
  }

  function openModelSettings(model) {
    openModal(model);
  }

  /** 关闭模态框 */
  function closeModal() {
    showModal.value = false;
  }

  async function load() {
    try {
      const d = await api("/models", "GET", null, t);
      models.value = d.data || [];
    } catch (e) {
      showToast(e.message, "error");
    }
  }

  async function syncCatalog() {
    syncing.value = true;
    try {
      const d = await api("/models/sync", "POST", null, t);
      const warnings = d.warnings?.length ? " / " + d.warnings.join("; ") : "";
      showToast(
        `${t("models.syncDone") || "同步完成"}: ${d.created_count || 0}+ / ${d.updated_count || 0}↻${warnings}`
      );
      await load();
    } catch (e) {
      showToast(e.message, "error");
    } finally {
      syncing.value = false;
    }
  }

  function buildPayload() {
    const pricing = {};
    if (String(newModel.prompt_price || "").trim()) pricing.prompt = String(newModel.prompt_price).trim();
    if (String(newModel.completion_price || "").trim()) pricing.completion = String(newModel.completion_price).trim();
    if (String(newModel.cache_read_price || "").trim()) pricing.input_cache_read = String(newModel.cache_read_price).trim();
    if (String(newModel.cache_write_price || "").trim()) pricing.input_cache_write = String(newModel.cache_write_price).trim();
    const payload = {
      id: newModel.id,
      name: newModel.name,
      upstream: newModel.upstream || "go",
      protocol: newModel.protocol,
      real_model: newModel.real_model || newModel.id,
      status: Number(newModel.status) === 0 ? 0 : 1,
      priority: Number(newModel.priority || 0),
    };
    if (editingId.value || Number(newModel.context_len || 0) > 0) {
      payload.context_len = Number(newModel.context_len || 0);
    }
    const tags = parseTags(newModel.tags_text);
    if (editingId.value || tags.length) {
      payload.tags = tags;
    }
    if (Object.keys(pricing).length) {
      payload.pricing = pricing;
    }
    return payload;
  }

  async function add() {
    if (!validateRequired(newModel.id, t("models.modelId"), t, showToast)) return;
    if (!validateRequired(newModel.protocol, t("models.protocol"), t, showToast)) return;
    try {
      const payload = buildPayload();
      if (editingId.value) {
        await api("/models/" + encodeURIComponent(editingId.value), "PATCH", payload, t);
        showToast(t("models.updateBtn") + " ✓");
      } else {
        await api("/models", "POST", payload, t);
        showToast(t("models.addBtn") + " ✓");
      }
      closeModal();
      load();
    } catch (e) {
      showToast(e.message, "error");
    }
  }

  async function toggle(id) {
    try {
      await api("/models/" + encodeURIComponent(id) + "/toggle", "POST", null, t);
      await load();
    } catch (e) {
      showToast(e.message, "error");
      await load();
    }
  }

  async function remove(id) {
    const item = models.value.find((m) => m.id === id);
    const name = item ? item.name || item.id : "";
    showConfirm(
      "deleteModel",
      async () => {
        try {
          await api("/models/" + encodeURIComponent(id), "DELETE", null, t);
          showToast(t("models.delete") + " ✓");
          load();
        } catch (e) {
          showToast(e.message, "error");
        }
      },
      name
    );
  }

  function parseTags(raw) {
    return String(raw || "")
      .split(/[，,\s]+/)
      .map((s) => s.trim().toLowerCase())
      .filter(Boolean);
  }

  return {
    models,
    newModel,
    showModal,
    editingId,
    syncing,
    availableModels,
    openModal,
    openModelSettings,
    closeModal,
    load,
    syncCatalog,
    add,
    toggle,
    remove,
  };
}
