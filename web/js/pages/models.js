/**
 * 模型路由管理页面 - 组合式函数
 * 支持模态框添加、下拉选择、自动填写模型ID
 */

import { validateRequired } from "../api.js?v=20260619a";
const { ref, reactive, computed, watch } = Vue;

/**
 * 根据 Go 可选模型列表生成路由
 */
const GO_MODELS = [
  { id: "glm-5.1", name: "GLM-5.1", protocol: "chat" },
  { id: "glm-5", name: "GLM-5", protocol: "chat" },
  { id: "kimi-k2.7-code", name: "Kimi K2.7 Code", protocol: "chat" },
  { id: "kimi-k2.6", name: "Kimi K2.6", protocol: "chat" },
  { id: "deepseek-v4-pro", name: "DeepSeek V4 Pro", protocol: "chat" },
  { id: "deepseek-v4-flash", name: "DeepSeek V4 Flash", protocol: "chat" },
  { id: "mimo-v2.5", name: "MiMo-V2.5", protocol: "chat" },
  { id: "mimo-v2.5-pro", name: "MiMo-V2.5-Pro", protocol: "chat" },
  { id: "minimax-m3", name: "MiniMax M3", protocol: "messages" },
  { id: "minimax-m2.7", name: "MiniMax M2.7", protocol: "messages" },
  { id: "minimax-m2.5", name: "MiniMax M2.5", protocol: "messages" },
  { id: "qwen3.7-max", name: "Qwen3.7 Max", protocol: "messages" },
  { id: "qwen3.7-plus", name: "Qwen3.7 Plus", protocol: "messages" },
  { id: "qwen3.6-plus", name: "Qwen3.6 Plus", protocol: "messages" },
];

export function useModels(api, showToast, t, showConfirm) {
  const models = ref([]);
  const showModal = ref(false);

  const newModel = reactive({
    id: "",
    name: "",
    upstream: "go",
    protocol: "chat",
    real_model: "",
    context_len: 0,
  });

  /** 当前上游可选的模型列表 */
  const availableModels = computed(() => GO_MODELS);

  /** 当 Go 模型变化时，自动同步模型 ID、名称与协议。 */
  watch(
    () => newModel.real_model,
    (val) => {
      const found = GO_MODELS.find((m) => m.id === val);
      if (!found) return;
      newModel.id = found.id;
      newModel.name = found.name;
      newModel.protocol = found.protocol;
      newModel.upstream = "go";
    }
  );

  /** 当名称变化时，自动填充模型 ID */
  watch(
    () => newModel.name,
    (val) => {
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

  /** 打开模态框 */
  function openModal() {
    newModel.id = "";
    newModel.name = "";
    newModel.upstream = "go";
    newModel.protocol = "chat";
    newModel.real_model = "";
    newModel.context_len = 0;
    showModal.value = true;
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

  async function add() {
    if (!validateRequired(newModel.id, t("models.modelId"), t, showToast))
      return;
    if (
      !validateRequired(newModel.protocol, t("models.protocol"), t, showToast)
    )
      return;
    try {
      newModel.upstream = "go";
      await api("/models", "POST", newModel, t);
      showToast(t("models.addBtn") + " ✓");
      closeModal();
      load();
    } catch (e) {
      showToast(e.message, "error");
    }
  }

  async function remove(id) {
    const item = models.value.find((m) => m.id === id);
    const name = item ? item.name || item.id : "";
    showConfirm(
      "deleteModel",
      async () => {
        try {
          await api("/models/" + id, "DELETE", null, t);
          showToast(t("models.delete") + " ✓");
          load();
        } catch (e) {
          showToast(e.message, "error");
        }
      },
      name
    );
  }

  return {
    models,
    newModel,
    showModal,
    availableModels,
    openModal,
    closeModal,
    load,
    add,
    remove,
  };
}
