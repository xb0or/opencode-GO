/**
 * 模型映射管理页面 - 组合式函数
 * 支持添加/修改/删除 client model -> upstream model 改写规则。
 */

import { validateRequired } from "../api.js";
const { ref, reactive } = Vue;

export function useMappings(api, showToast, t, showConfirm) {
  const mappings = ref([]);
  const showMappingModal = ref(false);
  const editingSource = ref("");

  const newMapping = reactive({
    source_model: "",
    target_model: "",
  });

  function openMappingModal() {
    editingSource.value = "";
    newMapping.source_model = "";
    newMapping.target_model = "";
    showMappingModal.value = true;
  }

  function openMappingSettings(mapping) {
    editingSource.value = mapping.source_model;
    newMapping.source_model = mapping.source_model;
    newMapping.target_model = mapping.target_model;
    showMappingModal.value = true;
  }

  function closeMappingModal() {
    showMappingModal.value = false;
  }

  async function load() {
    try {
      const d = await api("/model-mappings", "GET", null, t);
      mappings.value = d.data || [];
    } catch (e) {
      showToast(e.message, "error");
    }
  }

  async function add() {
    if (
      !validateRequired(
        newMapping.source_model,
        t("mappings.sourceModel"),
        t,
        showToast
      )
    )
      return;
    if (
      !validateRequired(
        newMapping.target_model,
        t("mappings.targetModel"),
        t,
        showToast
      )
    )
      return;

    try {
      if (
        editingSource.value &&
        editingSource.value !== newMapping.source_model
      ) {
        await api(
          "/model-mappings/" + encodeURIComponent(editingSource.value),
          "DELETE",
          null,
          t
        );
      }
      await api("/model-mappings", "POST", newMapping, t);
      showToast(
        (editingSource.value ? t("mappings.updateBtn") : t("mappings.addBtn")) +
          " ✓"
      );
      closeMappingModal();
      load();
    } catch (e) {
      showToast(e.message, "error");
    }
  }

  async function remove(source) {
    const item = mappings.value.find((m) => m.source_model === source);
    const name = item ? item.source_model + " → " + item.target_model : source;
    showConfirm(
      "deleteMapping",
      async () => {
        try {
          await api(
            "/model-mappings/" + encodeURIComponent(source),
            "DELETE",
            null,
            t
          );
          showToast(t("mappings.delete") + " ✓");
          load();
        } catch (e) {
          showToast(e.message, "error");
        }
      },
      name
    );
  }

  return {
    mappings,
    newMapping,
    editingSource,
    showMappingModal,
    openMappingModal,
    openMappingSettings,
    closeMappingModal,
    load,
    add,
    remove,
  };
}
