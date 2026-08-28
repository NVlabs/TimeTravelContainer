// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

#include "monitor_layout.h"

#define MONITOR_CODE                                                        \
  __attribute__((section("__TEXT,__ttcmon"), used, noinline,               \
                 visibility("default")))
#define MONITOR_NORETURN __attribute__((noreturn))
#define MONITOR_INLINE __attribute__((always_inline)) inline

#define PTE_VALID UINT64_C(1)
#define PTE_TABLE UINT64_C(2)
#define PTE_ADDR_MASK UINT64_C(0x0000fffffffff000)
#define PTE_AP_EL0 UINT64_C(1) << 6
#define PTE_AP_RO UINT64_C(1) << 7
#define PTE_SH_INNER UINT64_C(3) << 8
#define PTE_AF UINT64_C(1) << 10
#define PTE_NG UINT64_C(1) << 11
#define PTE_PXN UINT64_C(1) << 53
#define PTE_UXN UINT64_C(1) << 54

#define TTBR1_CODE_BASE UINT64_C(0xffff000000000000)

extern void ttc_monitor_hvc_boot(void) MONITOR_NORETURN;
extern void ttc_monitor_hvc_command(void) MONITOR_NORETURN;
extern void ttc_monitor_hvc_internal(void) MONITOR_NORETURN;
extern void ttc_monitor_dsb_ishst(void);
extern void ttc_monitor_dsb_ish(void);
extern void ttc_monitor_isb(void);
extern void ttc_monitor_prepare_mmu(void);
extern void ttc_monitor_tlbi_vae1is(uint64_t);
extern void ttc_monitor_tlbi_aside1is(uint64_t);
extern void ttc_monitor_write_ttbr0(uint64_t);

static MONITOR_INLINE uint16_t load16(const uint8_t* p) {
  return (uint16_t)p[0] | (uint16_t)p[1] << 8;
}

static MONITOR_INLINE uint32_t load32(const uint8_t* p) {
  return (uint32_t)p[0] | (uint32_t)p[1] << 8 | (uint32_t)p[2] << 16 |
         (uint32_t)p[3] << 24;
}

static MONITOR_INLINE uint64_t load64(const uint8_t* p) {
  return (uint64_t)load32(p) | (uint64_t)load32(p + 4) << 32;
}

static MONITOR_INLINE void zero_page(uint64_t* page) {
  for (uint64_t i = 0; i < TTC_MONITOR_PAGE_SIZE / sizeof(uint64_t); ++i) {
    page[i] = 0;
  }
}

static MONITOR_INLINE int bytes_zero(const uint8_t* p, uint64_t length) {
  for (uint64_t i = 0; i < length; ++i) {
    if (p[i] != 0) {
      return 0;
    }
  }
  return 1;
}

// Before the MMU is enabled, guest virtual addresses are IPAs. Bootstrap uses
// only the initial contiguous pool region and writes every initial TTBR1 PTE.
static uint64_t MONITOR_CODE boot_alloc_page(struct ttc_monitor_control* ctl) {
  struct ttc_monitor_pool_region* region = &ctl->pool[0];
  uint64_t used = (region->used + TTC_MONITOR_PAGE_SIZE - 1) &
                  ~(TTC_MONITOR_PAGE_SIZE - 1);
  if (used + TTC_MONITOR_PAGE_SIZE > region->length) {
    return 0;
  }
  uint64_t gpa = region->gpa + used;
  region->used = used + TTC_MONITOR_PAGE_SIZE;
  zero_page((uint64_t*)(uintptr_t)gpa);
  return gpa;
}

static int MONITOR_CODE boot_map_page(struct ttc_monitor_control* ctl,
                                      uint64_t va, uint64_t gpa,
                                      uint64_t leaf_flags) {
  uint64_t table_gpa = ctl->ttbr1_gpa;
  for (uint64_t level = 0; level < 3; ++level) {
    uint64_t* table = (uint64_t*)(uintptr_t)table_gpa;
    uint64_t index = (va >> (39 - level * 9)) & 511;
    uint64_t entry = table[index];
    if ((entry & (PTE_VALID | PTE_TABLE)) != (PTE_VALID | PTE_TABLE)) {
      uint64_t child = boot_alloc_page(ctl);
      if (child == 0) {
        return 0;
      }
      table[index] = child | PTE_VALID | PTE_TABLE;
      entry = table[index];
    }
    table_gpa = entry & PTE_ADDR_MASK;
  }
  uint64_t* leaf = (uint64_t*)(uintptr_t)table_gpa;
  leaf[(va >> 12) & 511] = (gpa & PTE_ADDR_MASK) | leaf_flags |
                           PTE_VALID | PTE_TABLE | PTE_AF | PTE_SH_INNER;
  return 1;
}

static int MONITOR_CODE boot_map_range(struct ttc_monitor_control* ctl,
                                       uint64_t va, uint64_t gpa,
                                       uint64_t length, uint64_t flags) {
  for (uint64_t offset = 0; offset < length; offset += TTC_MONITOR_PAGE_SIZE) {
    if (!boot_map_page(ctl, va + offset, gpa + offset, flags)) {
      return 0;
    }
  }
  return 1;
}

// ttc_monitor_bootstrap is entered at EL1 with stage-1 translation disabled.
// x0 points to the physical control page. It constructs the monitor's private
// 4K TTBR1 and exits before the host enables the MMU on this vCPU.
void MONITOR_CODE ttc_monitor_bootstrap(struct ttc_monitor_control* ctl) {
  if (ctl == 0 || ctl->magic != TTC_MONITOR_MAGIC ||
      ctl->version != TTC_MONITOR_VERSION || ctl->pool_region_count == 0 ||
      ctl->pool[0].gpa == 0 || ctl->pool[0].length < TTC_MONITOR_HOST_SLAB_SIZE) {
    ttc_monitor_hvc_internal();
  }
  if (ctl->ttbr1_gpa == 0) {
    ctl->ttbr1_gpa = boot_alloc_page(ctl);
    if (ctl->ttbr1_gpa == 0) {
      ttc_monitor_hvc_internal();
    }
    const uint64_t code_flags = PTE_AP_RO | PTE_NG;
    const uint64_t data_flags = PTE_PXN | PTE_UXN | PTE_NG;
    if (!boot_map_range(ctl, ctl->code_va, ctl->code_gpa,
                        ctl->code_length, code_flags) ||
        !boot_map_range(ctl, ctl->control_va, ctl->control_gpa,
                        TTC_MONITOR_HOST_SLAB_SIZE, data_flags) ||
        !boot_map_range(ctl, ctl->states_va, ctl->states_gpa,
                        ctl->state_count * TTC_MONITOR_HOST_SLAB_SIZE,
                        data_flags)) {
      ttc_monitor_hvc_internal();
    }
    for (uint64_t i = 0; i < ctl->pool_region_count; ++i) {
      // Bootstrap maps the complete reserved pool aperture once. Future pool
      // expansion only adds a permanent stage-2 lease and publishes it to the
      // allocator; no host-written PTE or emergency allocation is required.
      uint64_t length = i == 0 ? ctl->pool_aperture_length : 0;
      if (length != 0 &&
          !boot_map_range(ctl, ctl->pool[i].va, ctl->pool[i].gpa,
                          length, data_flags)) {
          ttc_monitor_hvc_internal();
        }
    }
  }
  ttc_monitor_dsb_ishst();
  // This vCPU has executed the bootstrap with stage-1 and both caches off.
  // Invalidate its instruction cache before the host enables SCTLR.I; without
  // this, a first exception into a monitor page not touched by bootstrap may
  // execute stale pre-copy contents indefinitely.
  ttc_monitor_prepare_mmu();
  ttc_monitor_isb();
  ttc_monitor_hvc_boot();
}

static MONITOR_INLINE struct ttc_monitor_control* control_from_state(
    uint8_t* state) {
  return (struct ttc_monitor_control*)(uintptr_t)load64(
      state + TTC_STATE_CONTROL_VA_OFFSET);
}

static MONITOR_INLINE uint64_t gpa_to_va(struct ttc_monitor_control* ctl,
                                         uint64_t gpa) {
  for (uint64_t i = 0; i < ctl->pool_region_count; ++i) {
    struct ttc_monitor_pool_region* region = &ctl->pool[i];
    if (gpa >= region->gpa && gpa - region->gpa < region->length) {
      return region->va + (gpa - region->gpa);
    }
  }
  return 0;
}

static uint64_t MONITOR_CODE alloc_page(struct ttc_monitor_control* ctl) {
  if (ctl->free_page_gpa != 0) {
    uint64_t gpa = ctl->free_page_gpa;
    uint64_t va = gpa_to_va(ctl, gpa);
    if (va == 0) {
      return 0;
    }
    ctl->free_page_gpa = *(uint64_t*)(uintptr_t)va;
    zero_page((uint64_t*)(uintptr_t)va);
    return gpa;
  }
  for (uint64_t i = 0; i < ctl->pool_region_count; ++i) {
    struct ttc_monitor_pool_region* region = &ctl->pool[i];
    uint64_t used = (region->used + TTC_MONITOR_PAGE_SIZE - 1) &
                    ~(TTC_MONITOR_PAGE_SIZE - 1);
    if (used + TTC_MONITOR_PAGE_SIZE <= region->length) {
      uint64_t gpa = region->gpa + used;
      region->used = used + TTC_MONITOR_PAGE_SIZE;
      uint64_t va = region->va + used;
      zero_page((uint64_t*)(uintptr_t)va);
      return gpa;
    }
  }
  return 0;
}

static int MONITOR_CODE recycle_page(struct ttc_monitor_control* ctl,
                                     uint64_t gpa) {
  uint64_t va = gpa_to_va(ctl, gpa);
  if (va == 0) {
    return 0;
  }
  uint64_t* page = (uint64_t*)(uintptr_t)va;
  zero_page(page);
  page[0] = ctl->free_page_gpa;
  ctl->free_page_gpa = gpa;
  return 1;
}

static int MONITOR_CODE recycle_table(struct ttc_monitor_control* ctl,
                                      uint64_t table_gpa, uint64_t level) {
  uint64_t table_va = gpa_to_va(ctl, table_gpa);
  if (table_va == 0 || level > 3) {
    return 0;
  }
  uint64_t* table = (uint64_t*)(uintptr_t)table_va;
  if (level < 3) {
    for (uint64_t i = 0; i < 512; ++i) {
      uint64_t entry = table[i];
      if ((entry & PTE_VALID) == 0) {
        continue;
      }
      if ((entry & (PTE_VALID | PTE_TABLE)) !=
          (PTE_VALID | PTE_TABLE)) {
        return 0;
      }
      if (!recycle_table(ctl, entry & PTE_ADDR_MASK, level + 1)) {
        return 0;
      }
    }
  }
  return recycle_page(ctl, table_gpa);
}

static struct ttc_monitor_address_space* MONITOR_CODE find_space(
    struct ttc_monitor_control* ctl, uint64_t id) {
  for (uint64_t i = 0; i < TTC_MONITOR_MAX_ADDRESS_SPACES; ++i) {
    uint8_t active = *(volatile uint8_t*)&ctl->spaces[i].active;
    if (active) {
      // Pair with create_space's release publication. Application entry runs
      // concurrently on other vCPUs and must not observe active before the
      // root, ASID, and ID are globally visible.
      __asm__ __volatile__("dmb ishld" ::: "memory");
    }
    if (active && ctl->spaces[i].id == id) {
      return &ctl->spaces[i];
    }
  }
  return 0;
}

static uint64_t* MONITOR_CODE find_leaf(struct ttc_monitor_control* ctl,
                                        uint64_t root_gpa, uint64_t va,
                                        int create) {
  uint64_t table_gpa = root_gpa;
  for (uint64_t level = 0; level < 3; ++level) {
    uint64_t table_va = gpa_to_va(ctl, table_gpa);
    if (table_va == 0) {
      return 0;
    }
    uint64_t* table = (uint64_t*)(uintptr_t)table_va;
    uint64_t index = (va >> (39 - level * 9)) & 511;
    uint64_t entry = table[index];
    if ((entry & (PTE_VALID | PTE_TABLE)) != (PTE_VALID | PTE_TABLE)) {
      if (!create) {
        return 0;
      }
      uint64_t child = alloc_page(ctl);
      if (child == 0) {
        return 0;
      }
      table[index] = child | PTE_VALID | PTE_TABLE;
      ttc_monitor_dsb_ishst();
      entry = table[index];
    }
    table_gpa = entry & PTE_ADDR_MASK;
  }
  uint64_t table_va = gpa_to_va(ctl, table_gpa);
  if (table_va == 0) {
    return 0;
  }
  return &((uint64_t*)(uintptr_t)table_va)[(va >> 12) & 511];
}

static MONITOR_INLINE uint64_t stage1_leaf(
    const struct ttc_monitor_address_space* space, uint64_t gpa,
    uint8_t access) {
  uint64_t pte = (gpa & PTE_ADDR_MASK) | PTE_VALID | PTE_TABLE | PTE_AF |
                 PTE_SH_INNER | PTE_NG;
  if ((space->flags & TTC_ADDRESS_SPACE_SENTRY) == 0) {
    // Application pages are accessible at EL0. EL1 must never execute them.
    pte |= PTE_AP_EL0 | PTE_PXN;
  } else {
    // Sentry pages are accessible only at EL1. EL0 must never execute them.
    pte |= PTE_UXN;
  }
  if ((access & TTC_ACCESS_WRITE) == 0) {
    pte |= PTE_AP_RO;
  }
  if ((access & TTC_ACCESS_EXECUTE) == 0) {
    pte |= PTE_PXN | PTE_UXN;
  }
  return pte;
}

static MONITOR_INLINE int leaf_executable(
    const struct ttc_monitor_address_space* space, uint64_t pte) {
  if (pte == 0) {
    return 0;
  }
  if ((space->flags & TTC_ADDRESS_SPACE_SENTRY) != 0) {
    return (pte & PTE_PXN) == 0;
  }
  return (pte & PTE_UXN) == 0;
}

static MONITOR_INLINE void clean_data_to_pou(uint64_t va, uint64_t length) {
  uint64_t ctr;
  __asm__ __volatile__("mrs %0, ctr_el0" : "=r"(ctr));
  uint64_t line_size = UINT64_C(4) << ((ctr >> 16) & 15);
  uint64_t end = va + length;
  for (uint64_t line = va & ~(line_size - 1); line < end;
       line += line_size) {
    __asm__ __volatile__("dc cvau, %0" : : "r"(line) : "memory");
  }
}

static MONITOR_INLINE void synchronize_instruction_caches(void) {
  // GuestRAM is populated through host mappings. Before granting EL0 execute,
  // clean each affected data-cache line to the point of unification and then
  // invalidate every inner-shareable PE's instruction cache. IALLUIS is broad,
  // but unlike IVAU it covers all fixed-pool vCPUs and is the correct v1
  // primitive when executable pages can migrate between workers.
  __asm__ __volatile__("dsb ish\n\tic ialluis\n\tdsb ish\n\tisb" ::: "memory");
}

static void MONITOR_CODE respond(uint8_t* state, uint64_t sequence,
                                  uint32_t status, uint32_t detail) {
  volatile uint8_t* response = state + TTC_STATE_RESPONSE_OFFSET;
  for (uint64_t i = 0; i < 32; ++i) {
    response[i] = 0;
  }
  // Read the already-validated magic from monitor-owned control memory. This
  // also prevents the freestanding compiler from emitting an out-of-image
  // Mach-O literal pool for the 64-bit constant.
  uint64_t magic = control_from_state(state)->magic;
  for (uint64_t i = 0; i < 8; ++i) {
    response[i] = magic >> (i * 8);
  }
  response[8] = TTC_MONITOR_VERSION;
  for (uint64_t i = 0; i < 8; ++i) {
    response[16 + i] = sequence >> (i * 8);
  }
  for (uint64_t i = 0; i < 4; ++i) {
    response[24 + i] = status >> (i * 8);
    response[28 + i] = detail >> (i * 8);
  }
}

static uint32_t MONITOR_CODE validate_header(uint8_t* command,
                                             uint64_t length,
                                             uint64_t* sequence,
                                             uint16_t* opcode,
                                             uint32_t* count,
                                             uint64_t* space_id,
                                             uint64_t* flags,
                                             uint64_t* pool_gpa,
                                             uint64_t* pool_length) {
  if (length < 64 || load64(command) != TTC_MONITOR_MAGIC ||
      load16(command + 8) != TTC_MONITOR_VERSION ||
      !bytes_zero(command + 56, 8)) {
    return TTC_STATUS_MALFORMED;
  }
  *opcode = load16(command + 10);
  *count = load32(command + 12);
  *sequence = load64(command + 16);
  *space_id = load64(command + 24);
  *flags = load64(command + 32);
  *pool_gpa = load64(command + 40);
  *pool_length = load64(command + 48);
  if (*sequence == 0 || *count > TTC_MONITOR_MAX_OPERATIONS ||
      length != 64 + (uint64_t)*count * 40) {
    return TTC_STATUS_MALFORMED;
  }
  return TTC_STATUS_OK;
}

static uint32_t MONITOR_CODE expand_pool(struct ttc_monitor_control* ctl,
                                         uint64_t gpa, uint64_t length) {
  if (ctl->pool_region_count == 0 ||
      ctl->pool_region_count >= TTC_MONITOR_MAX_POOL_REGIONS ||
      (gpa & (TTC_MONITOR_HOST_SLAB_SIZE - 1)) != 0 || length == 0 ||
      (length & (TTC_MONITOR_HOST_SLAB_SIZE - 1)) != 0 ||
      gpa + length < gpa) {
    return TTC_STATUS_MALFORMED;
  }
  struct ttc_monitor_pool_region* first = &ctl->pool[0];
  uint64_t expected_gpa = first->gpa;
  for (uint64_t i = 0; i < ctl->pool_region_count; ++i) {
    if (ctl->pool[i].gpa != expected_gpa || ctl->pool[i].length == 0 ||
        expected_gpa + ctl->pool[i].length < expected_gpa) {
      return TTC_STATUS_INTERNAL;
    }
    expected_gpa += ctl->pool[i].length;
  }
  if (gpa != expected_gpa || gpa - first->gpa > ctl->pool_aperture_length ||
      length > ctl->pool_aperture_length - (gpa - first->gpa)) {
    return TTC_STATUS_CONFLICT;
  }
  struct ttc_monitor_pool_region* region =
      &ctl->pool[ctl->pool_region_count];
  region->gpa = gpa;
  region->va = first->va + (gpa - first->gpa);
  region->length = length;
  region->used = 0;
  ttc_monitor_dsb_ishst();
  ++ctl->pool_region_count;
  ctl->next_pool_va = region->va + length;
  ttc_monitor_dsb_ish();
  ttc_monitor_isb();
  return TTC_STATUS_OK;
}

static uint32_t MONITOR_CODE create_space(struct ttc_monitor_control* ctl,
                                          uint64_t id, uint64_t flags) {
  if (id == 0 || (flags & ~TTC_ADDRESS_SPACE_FLAGS_MASK) != 0 ||
      find_space(ctl, id) != 0) {
    return TTC_STATUS_CONFLICT;
  }
  for (uint64_t i = 0; i < TTC_MONITOR_MAX_ADDRESS_SPACES; ++i) {
    if (!ctl->spaces[i].active) {
      uint64_t root = alloc_page(ctl);
      if (root == 0) {
        return TTC_STATUS_NO_MEMORY;
      }
      uint64_t asid = ctl->next_asid++;
      if (asid == 0 || asid > UINT16_MAX) {
        asid = 1;
        ctl->next_asid = 2;
        // ASID reuse is safe only after flushing all stage-1 translations.
        __asm__ __volatile__("tlbi vmalle1is\n\tdsb ish\n\tisb" ::: "memory");
      }
      ctl->spaces[i].id = id;
      ctl->spaces[i].root_gpa = root;
      ctl->spaces[i].asid = (uint16_t)asid;
      ctl->spaces[i].flags = (uint32_t)flags;
      ttc_monitor_dsb_ishst();
      *(volatile uint8_t*)&ctl->spaces[i].active = 1;
      ttc_monitor_dsb_ish();
      return TTC_STATUS_OK;
    }
  }
  return TTC_STATUS_NO_MEMORY;
}

static uint32_t MONITOR_CODE destroy_space(struct ttc_monitor_control* ctl,
                                           uint64_t id) {
  struct ttc_monitor_address_space* space = find_space(ctl, id);
  if (space == 0) {
    return TTC_STATUS_NOT_FOUND;
  }
  ttc_monitor_dsb_ishst();
  ttc_monitor_tlbi_aside1is((uint64_t)space->asid << 48);
  ttc_monitor_dsb_ish();
  ttc_monitor_isb();
  uint64_t root_gpa = space->root_gpa;
  *(volatile uint8_t*)&space->active = 0;
  ttc_monitor_dsb_ish();
  space->id = 0;
  space->root_gpa = 0;
  space->asid = 0;
  space->flags = 0;
  // The host mutation rendezvous has stopped application vCPUs, and the ASID
  // invalidation above is acknowledged. No page walker can retain a reference
  // to this tree, so recursively return every table page to the intrusive free
  // list. GPAs remain permanently leased stage-2 memory; only monitor-owned
  // stage-1 metadata is recycled.
  if (!recycle_table(ctl, root_gpa, 0)) {
    return TTC_STATUS_INTERNAL;
  }
  return TTC_STATUS_OK;
}

static uint32_t MONITOR_CODE apply(struct ttc_monitor_control* ctl,
                                   struct ttc_monitor_address_space* space,
                                   const uint8_t* operations, uint32_t count,
                                   uint32_t* detail) {
  uint32_t mutation_count = 0;
  for (uint32_t op_index = 0; op_index < count; ++op_index) {
    const uint8_t* op = operations + (uint64_t)op_index * 40;
    uint8_t type = op[0];
    uint8_t access = op[1];
    uint64_t va = load64(op + 8);
    uint64_t gpa = load64(op + 16);
    uint64_t length = load64(op + 24);
    if (!bytes_zero(op + 2, 6) || !bytes_zero(op + 32, 8) ||
        (access & ~(TTC_ACCESS_READ | TTC_ACCESS_WRITE |
                    TTC_ACCESS_EXECUTE)) != 0 ||
        (va & (TTC_MONITOR_PAGE_SIZE - 1)) != 0 || length == 0 ||
        (length & (TTC_MONITOR_PAGE_SIZE - 1)) != 0 ||
        va + length < va) {
      *detail = op_index;
      return TTC_STATUS_MALFORMED;
    }
    uint64_t pages = length / TTC_MONITOR_PAGE_SIZE;
    if (pages > TTC_MONITOR_MAX_OPERATIONS - mutation_count) {
      *detail = op_index;
      return TTC_STATUS_MALFORMED;
    }
    if (type == TTC_OPERATION_MAP) {
      if (access == 0 || (gpa & (TTC_MONITOR_PAGE_SIZE - 1)) != 0 ||
          gpa + length < gpa) {
        *detail = op_index;
        return TTC_STATUS_MALFORMED;
      }
    } else if (type == TTC_OPERATION_UNMAP) {
      if (access != 0 || gpa != 0) {
        *detail = op_index;
        return TTC_STATUS_MALFORMED;
      }
    } else if (type == TTC_OPERATION_PROTECT) {
      if (access == 0 || gpa != 0) {
        *detail = op_index;
        return TTC_STATUS_MALFORMED;
      }
    } else {
      *detail = op_index;
      return TTC_STATUS_UNSUPPORTED;
    }
    for (uint64_t page = 0; page < pages; ++page) {
      uint64_t page_va = va + page * TTC_MONITOR_PAGE_SIZE;
      uint64_t* leaf = find_leaf(ctl, space->root_gpa, page_va,
                                 type == TTC_OPERATION_MAP);
      if (leaf == 0) {
        if (type == TTC_OPERATION_MAP) {
          *detail = op_index;
          return TTC_STATUS_NO_MEMORY;
        }
        if (type == TTC_OPERATION_PROTECT) {
          *detail = op_index;
          return TTC_STATUS_NOT_FOUND;
        }
        continue;
      }
      // Operations in one command are ordered. If an earlier operation already
      // planned a replacement for this leaf, use that pending descriptor as
      // the input to this operation rather than rereading the unchanged live
      // page table. This makes unmap+map and map+protect atomic batches behave
      // sequentially while still publishing one BBM/TLBI transaction.
      uint32_t mutation_index = mutation_count;
      uint64_t old = *leaf;
      for (uint32_t i = 0; i < mutation_count; ++i) {
        if (ctl->mutations[i].pte_va == (uint64_t)(uintptr_t)leaf) {
          mutation_index = i;
          old = ctl->mutations[i].new_pte;
          break;
        }
      }
      uint64_t next = 0;
      if (type == TTC_OPERATION_MAP) {
        next = stage1_leaf(space, gpa + page * TTC_MONITOR_PAGE_SIZE,
                           access);
      } else if (type == TTC_OPERATION_PROTECT) {
        if ((old & PTE_VALID) == 0) {
          *detail = op_index;
          return TTC_STATUS_NOT_FOUND;
        }
        next = stage1_leaf(space, old & PTE_ADDR_MASK, access);
      }
      if (old != next) {
        if (mutation_index == mutation_count) {
          ctl->mutations[mutation_count].pte_va =
              (uint64_t)(uintptr_t)leaf;
          ctl->mutations[mutation_count].new_pte = next;
          ctl->mutations[mutation_count].va = page_va;
          ++mutation_count;
        } else {
          ctl->mutations[mutation_index].new_pte = next;
        }
      }
    }
  }

  // Break every valid descriptor before replacement or permission change.
  for (uint32_t i = 0; i < mutation_count; ++i) {
    uint64_t* leaf = (uint64_t*)(uintptr_t)ctl->mutations[i].pte_va;
    if ((*leaf & PTE_VALID) != 0) {
      *leaf = 0;
    }
  }
  ttc_monitor_dsb_ishst();
  for (uint32_t i = 0; i < mutation_count; ++i) {
    uint64_t operand = ((uint64_t)space->asid << 48) |
                       (ctl->mutations[i].va >> 12);
    ttc_monitor_tlbi_vae1is(operand);
  }
  ttc_monitor_dsb_ish();
  ttc_monitor_isb();
  for (uint32_t i = 0; i < mutation_count; ++i) {
    uint64_t* leaf = (uint64_t*)(uintptr_t)ctl->mutations[i].pte_va;
    *leaf = ctl->mutations[i].new_pte;
  }
  ttc_monitor_dsb_ishst();
  uint32_t executable_mutations = 0;
  for (uint32_t i = 0; i < mutation_count; ++i) {
    if (leaf_executable(space, ctl->mutations[i].new_pte)) {
      ++executable_mutations;
    }
  }
  if (executable_mutations != 0) {
    // Cache maintenance by VA uses the affected stage-1 address space. This
    // vCPU may most recently have run an unrelated application address space.
    ttc_monitor_write_ttbr0(space->root_gpa |
                            ((uint64_t)space->asid << 48));
    for (uint32_t i = 0; i < mutation_count; ++i) {
      if (leaf_executable(space, ctl->mutations[i].new_pte)) {
        clean_data_to_pou(ctl->mutations[i].va, TTC_MONITOR_PAGE_SIZE);
      }
    }
    synchronize_instruction_caches();
  } else {
    ttc_monitor_isb();
  }
  return TTC_STATUS_OK;
}

static void MONITOR_CODE execute_command(uint8_t* state) {
  struct ttc_monitor_control* ctl = control_from_state(state);
  uint8_t* command = state + TTC_STATE_COMMAND_OFFSET;
  uint64_t length = load32(state + TTC_STATE_COMMAND_LENGTH_OFFSET);
  uint64_t sequence = 0;
  uint64_t space_id = 0;
  uint64_t flags = 0;
  uint64_t pool_gpa = 0;
  uint64_t pool_length = 0;
  uint16_t opcode = 0;
  uint32_t count = 0;
  uint32_t detail = 0;
  uint32_t status = validate_header(command, length, &sequence, &opcode,
                                    &count, &space_id, &flags, &pool_gpa,
                                    &pool_length);
  if (status != TTC_STATUS_OK || ctl == 0 ||
      ctl->magic != TTC_MONITOR_MAGIC) {
    respond(state, sequence, status == TTC_STATUS_OK ? TTC_STATUS_INTERNAL
                                                     : status,
            detail);
    return;
  }
  if (opcode == TTC_OPCODE_CREATE_ADDRESS_SPACE) {
    if (count != 0 || (flags & ~TTC_ADDRESS_SPACE_FLAGS_MASK) != 0 ||
        space_id == 0 || pool_gpa != 0 || pool_length != 0) {
      status = TTC_STATUS_MALFORMED;
    } else {
      status = create_space(ctl, space_id, flags);
    }
  } else if (opcode == TTC_OPCODE_DESTROY_ADDRESS_SPACE) {
    if (count != 0 || flags != 0 || space_id == 0 || pool_gpa != 0 ||
        pool_length != 0) {
      status = TTC_STATUS_MALFORMED;
    } else {
      status = destroy_space(ctl, space_id);
    }
  } else if (opcode == TTC_OPCODE_ACTIVATE) {
    struct ttc_monitor_address_space* space = find_space(ctl, space_id);
    if (count != 0 || flags != 0 || space_id == 0 || pool_gpa != 0 ||
        pool_length != 0) {
      status = TTC_STATUS_MALFORMED;
    } else if (space == 0) {
      status = TTC_STATUS_NOT_FOUND;
    } else {
      // Activation validates that the root still exists. Sentry-at-EL1
      // application entry selects and installs it immediately before ERET.
      status = TTC_STATUS_OK;
    }
  } else if (opcode == TTC_OPCODE_APPLY) {
    struct ttc_monitor_address_space* space = find_space(ctl, space_id);
    if (count == 0 || flags != 0 || space_id == 0 || pool_gpa != 0 ||
        pool_length != 0) {
      status = TTC_STATUS_MALFORMED;
    } else if (space == 0) {
      status = TTC_STATUS_NOT_FOUND;
    } else {
      status = apply(ctl, space, command + 64, count, &detail);
    }
  } else if (opcode == TTC_OPCODE_EXPAND_PAGE_TABLE_POOL) {
    if (count != 0 || flags != 0 || space_id != 0) {
      status = TTC_STATUS_MALFORMED;
    } else {
      status = expand_pool(ctl, pool_gpa, pool_length);
    }
  } else {
    status = TTC_STATUS_UNSUPPORTED;
  }
  respond(state, sequence, status, detail);
}

void MONITOR_CODE ttc_monitor_command_entry(uint8_t* state) {
  execute_command(state);
  // Publish both the command response and any monitor metadata mutation to
  // the host and to application entries running on other vCPUs before the
  // synchronous acknowledgement becomes observable.
  ttc_monitor_dsb_ish();
  ttc_monitor_hvc_command();
}

// ttc_monitor_ring0_application_ttbr validates the application root selected
// by a Sentry continuation. The assembly wrapper saves the complete Sentry
// context before calling this helper and performs the TTBR0 switch only after
// the helper has returned, so its C stack remains addressable throughout.
uint64_t MONITOR_CODE ttc_monitor_ring0_application_ttbr(uint8_t* state) {
  struct ttc_monitor_control* ctl = control_from_state(state);
  uint64_t id = load64(state + TTC_STATE_ADDRESS_SPACE_ID_OFFSET);
  struct ttc_monitor_address_space* space = find_space(ctl, id);
  if (space == 0 ||
      (space->flags & TTC_ADDRESS_SPACE_SENTRY) != 0) {
    return 0;
  }
  return space->root_gpa | ((uint64_t)space->asid << 48);
}
