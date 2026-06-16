/**
 * 模型路由管理页面 - 组合式函数
 * 支持模态框添加、下拉选择、自动填写模型ID
 */

import { validateRequired } from "../api.js";
const { ref, reactive, computed, watch } = Vue;

/**
 * 根据上游可选的真实模型列表
 */
const MODEL_OPTIONS = {
  go: [
    "gpt-4o",
    "gpt-4o-mini",
    "claude-sonnet-4-20250514",
    "claude-3-5-haiku-latest",
    "gemini-2.5-pro-exp-03-25",
    "gemini-2.0-flash-exp",
    "deepseek-chat",
    "deepseek-reasoner",
  ],
};

const GROUP_OPTIONS = ["go"];

export function useModels(api, showToast, t, showConfirm) {
  const models = ref([]);
  const showModal = ref(false);

  const newModel = reactive({
    id: "",
    name: "",
    upstream: "go",
    protocol: "chat",
    real_model: "",
    group: "go",
    context_len: 0,
  });

  /** 当前上游可选的模型列表 */
  const availableModels = computed(
    () => MODEL_OPTIONS[newModel.upstream] || []
  );
  const availableGroups = GROUP_OPTIONS;

  /** 当上游切换时，自动同步分组 */
  watch(
    () => newModel.upstream,
    (val) => {
      if (GROUP_OPTIONS.includes(val)) {
        newModel.group = val;
      }
    }
  );

  /** 当名称变化时，自动填充模型 ID（格式: {upstream}-{slug}） */
  watch(
    () => newModel.name,
    (val) => {
      if (val && val.trim()) {
        newModel.id = newModel.upstream + "-" + toSlug(val);
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
    newModel.group = "go";
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
    availableGroups,
    openModal,
    closeModal,
    load,
    add,
    remove,
  };
}
