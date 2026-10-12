<script setup lang="ts">
import { computed, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'

// 敏感字段输入框（F17）。
//
// 默认遮蔽，需要再次确认才能显示明文。确认不是形式主义：
//   - 明文只存在于浏览器内存，服务端不落库、不回传（/api/pkg/*/preview 返回的
//     TOML 里这些值已被遮蔽）；
//   - 生成出来的 ZIP 内含明文密钥，属于敏感文件，管理员应当知道自己在看什么。
// 复制也走同一道确认，避免「顺手 Ctrl+C 就带出去了」。
//
// 遮蔽的呈现方式（曾经踩过）：
//   早先的写法是 `type="password"` + `#prefix` 插槽塞一个遮蔽串，结果是**同一个值
//   被画了两遍** —— 前缀里一个 `Dh71**********N8`，输入框本体又一串圆点，两段挤在
//   一行里，看着像渲染坏了。
//   现在的规则只有一条：**未显示明文且有值时，输入框里显示的就是遮蔽串本身**
//   （只读）；显示明文后才可编辑。这样界面上永远只有一种表示，且与
//   /api/pkg/*/preview 里 `maskSecret` 出来的写法完全一致。
//   值是空的（例如还没配接入点）没什么可遮，直接可编辑。

const props = defineProps<{
  modelValue: string
  label?: string
  placeholder?: string
  /** 说明文字，例如「服务器端与客户端必须一致」。 */
  hint?: string
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', v: string): void
}>()

const revealed = ref(false)

const value = computed(() => props.modelValue || '')

/** 遮蔽显示：保留前 4 位与后 2 位，与后端 maskSecret 的观感一致。 */
const masked = computed(() => {
  const v = value.value
  if (!v) return ''
  if (v.length <= 8) return '*'.repeat(v.length)
  return v.slice(0, 4) + '*'.repeat(10) + v.slice(-2)
})

/** 有值且未显示明文 => 显示遮蔽串，且只读（要改就先显示明文）。 */
const locked = computed(() => !revealed.value && value.value !== '')

/**
 * 输入框的显示值与真实值在此分叉：
 *   锁定时 get 返回遮蔽串，用户看到的、输入框里躺着的都是遮蔽串；
 *   一旦从空值开始输入（说明这是在录入新密钥，不是在看已有的），
 *   直接切到明文，省得每敲一个字都要先确认一次。
 */
const display = computed({
  get: () => (locked.value ? masked.value : value.value),
  set: (v: string) => {
    if (locked.value) return // 只读时本不该触发，兜底防止把遮蔽串写回真实值
    if (!revealed.value) revealed.value = true
    emit('update:modelValue', v)
  },
})

async function confirmReveal(action: string): Promise<boolean> {
  try {
    await ElMessageBox.confirm(
      `${action}后该值会以明文出现在界面上。请确认当前屏幕没有他人旁观，且不要把它粘贴到聊天工具或不安全的文件中。`,
      `确认${action}敏感值`,
      { confirmButtonText: `确认${action}`, cancelButtonText: '取消', type: 'warning' },
    )
    return true
  } catch {
    return false
  }
}

async function toggleReveal() {
  if (revealed.value) {
    revealed.value = false
    return
  }
  if (await confirmReveal('显示')) revealed.value = true
}

/** 点遮蔽串等于在问「这是什么」，那就走同一道确认流程，而不是让点击石沉大海。 */
function onFieldClick(e: MouseEvent) {
  if (!locked.value) return
  const t = e.target as HTMLElement | null
  // 右侧的显示/复制按钮各自有处理函数。注意 Element Plus 的 append 容器类名是
  // `.el-input-group__append` —— 写成 `.el-input__append` 匹配不上，会导致按钮点击
  // 之后又冒泡到这里、再走一次 toggleReveal，**叠出两个确认框**。
  if (t && t.closest('.el-input-group__append, .el-input__suffix')) return
  void toggleReveal()
}

async function copyValue() {
  if (!value.value) {
    ElMessage.warning('当前没有可复制的内容')
    return
  }
  if (!(await confirmReveal('复制'))) return
  try {
    await navigator.clipboard.writeText(value.value)
    ElMessage.success('已复制到剪贴板')
  } catch {
    ElMessage.error('复制失败：浏览器拒绝了剪贴板访问（仅在 HTTPS 或 localhost 下可用）')
  }
}

defineExpose({ revealed })
</script>

<template>
  <div
    class="sf-secret"
    :class="{ 'sf-secret-locked': locked }"
    :title="locked ? '点击可显示明文（需确认）' : ''"
    @click="onFieldClick"
  >
    <el-input
      v-model="display"
      :readonly="locked"
      :placeholder="placeholder"
      spellcheck="false"
      autocomplete="off"
    >
      <template #append>
        <el-button :title="revealed ? '隐藏明文' : '显示明文（需确认）'" @click="toggleReveal">
          <el-icon><component :is="revealed ? 'Hide' : 'View'" /></el-icon>
        </el-button>
        <el-button title="复制（需确认）" @click="copyValue">
          <el-icon><CopyDocument /></el-icon>
        </el-button>
      </template>
    </el-input>
    <div v-if="hint" class="sf-cell-sub" style="margin-top: 4px">{{ hint }}</div>
  </div>
</template>

<style scoped>
/* 密钥一律等宽，遮蔽串与明文切换时长度不会跳。 */
:deep(.el-input__inner) {
  font-family: 'Cascadia Mono', Consolas, monospace;
}
/* 遮蔽态给一点「这里不是给你直接改的」的暗示。 */
.sf-secret-locked :deep(.el-input__inner) {
  color: var(--sf-muted);
  cursor: pointer;
}

/* append 区里放了两个按钮（显示明文 / 复制）。
 *
 * Element Plus 给 `.el-input-group__append .el-button` 设的是 **负** 外边距
 * （目的是让「单个」按钮撑满整块 append）。一旦放进两个按钮，负边距就叠加，
 * 两个图标贴死成一团 —— 既看不出是两个动作，也容易点错。
 * 这里把外边距归零、改用 gap 拉开间距，再给 append 一点左右内边距。
 * 背景保持 Element Plus 的灰底，gap 处露出的灰底正好当分隔线。 */
:deep(.el-input-group__append) {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  padding: 0 10px;
}
:deep(.el-input-group__append .el-button) {
  margin: 0;
  padding: 7px 9px;
}
/* Element Plus 全局的 `.el-button + .el-button { margin-left: 12px }` 会与 gap 叠加，
 * 间距变成 20px，看着松散。统一交给 gap 控制。 */
:deep(.el-input-group__append .el-button + .el-button) {
  margin-left: 0;
}
</style>
