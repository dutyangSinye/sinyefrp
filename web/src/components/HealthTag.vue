<script setup lang="ts">
import { computed } from 'vue'
import type { Health } from '@/api/types'
import { describeHealth } from '@/utils/format'

// 统一展示采集健康度。
//
// 存在的意义是强制所有页面用同一种口径显示「未知」：
// 哪怕某个页面忘了判断 known，也用不到它自己的判断——因为这里已经处理了。

const props = defineProps<{
  health: Health | null | undefined
  /** 是否附带一行文字说明。 */
  showDetail?: boolean
}>()

const desc = computed(() => describeHealth(props.health))
</script>

<template>
  <el-tooltip :content="desc.detail" placement="top">
    <el-tag :type="desc.type" size="small" disable-transitions effect="light">
      {{ desc.label }}
    </el-tag>
  </el-tooltip>
  <span v-if="showDetail" class="sf-muted" style="margin-left: 8px; font-size: 12px">
    {{ desc.detail }}
  </span>
</template>
