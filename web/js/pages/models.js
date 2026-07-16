/**
 * 模型路由管理页面 - 组合式函数
 * 支持同步、启停、添加/编辑与自定义字段保护。
 */

import { validateRequired } from "../api.js?v=20260619a";
const { ref, reactive, computed, watch } = Vue;

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

  /**
   * 可选的模型列表：完全从 /admin/models (DB 同步后的数据) 获取。
   * 不再使用硬编码的种子列表——所有模型由 modelsync 从上游 API 动态发现。
   * 如果 DB 中已有模型，直接使用；如果 DB 为空（首次启动尚未同步），
   * 列表为空，用户需先点击「同步」按钮。
   */
  const availableModels = computed(() => {
    const byId = new Map();
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
