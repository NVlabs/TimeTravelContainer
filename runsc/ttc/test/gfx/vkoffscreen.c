// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// vkoffscreen checks a Vulkan device end to end without a display:
//
//   - a compute shader fills a host-visible buffer, which the host checks;
//   - a render pass clears a 64x64 image to blue and draws a red square whose
//     edges lie on pixel boundaries, so every pixel is determined; the image is
//     copied to a host-visible buffer, and the program prints its checksum.
//
// Usage: vkoffscreen COMPUTE.spv VERTEX.spv FRAGMENT.spv [ITERATIONS [DELAY_MS [rerecord]]]
// It exits 0 and prints "vkoffscreen: ok checksum=..." if every check passes.
// With ITERATIONS, it repeats the submission and the checks on one device,
// DELAY_MS apart, and prints "vkoffscreen: iteration N ok" after each. With
// rerecord, it records the command buffer again before each submission, as
// a renderer restored from a snapshot, which has the command buffer but not
// its commands, needs.
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <vulkan/vulkan.h>

#define CHECK(x)                                                              \
	do {                                                                      \
		VkResult r_ = (x);                                                    \
		if (r_ != VK_SUCCESS) {                                               \
			fprintf(stderr, "vkoffscreen: %s:%d: %s = %d\n", __FILE__,        \
			        __LINE__, #x, (int)r_);                                  \
			exit(1);                                                          \
		}                                                                     \
	} while (0)

enum { N = 4096, W = 64, H = 64 };

static VkDevice dev;
static VkPhysicalDevice phys;

static uint32_t *read_file(const char *path, size_t *size) {
	FILE *f = fopen(path, "rb");
	if (!f) {
		perror(path);
		exit(1);
	}
	fseek(f, 0, SEEK_END);
	*size = (size_t)ftell(f);
	fseek(f, 0, SEEK_SET);
	uint32_t *data = malloc(*size);
	if (fread(data, 1, *size, f) != *size) {
		perror(path);
		exit(1);
	}
	fclose(f);
	return data;
}

static VkShaderModule shader(const char *path) {
	size_t size;
	uint32_t *code = read_file(path, &size);
	VkShaderModuleCreateInfo info = {
	    .sType = VK_STRUCTURE_TYPE_SHADER_MODULE_CREATE_INFO,
	    .codeSize = size,
	    .pCode = code,
	};
	VkShaderModule module;
	CHECK(vkCreateShaderModule(dev, &info, NULL, &module));
	free(code);
	return module;
}

static uint32_t memory_type(uint32_t bits, VkMemoryPropertyFlags want) {
	VkPhysicalDeviceMemoryProperties props;
	vkGetPhysicalDeviceMemoryProperties(phys, &props);
	for (uint32_t i = 0; i < props.memoryTypeCount; i++) {
		if ((bits & (1u << i)) && (props.memoryTypes[i].propertyFlags & want) == want) {
			return i;
		}
	}
	fprintf(stderr, "vkoffscreen: no memory type %#x in %#x\n", want, bits);
	exit(1);
}

// buffer creates a buffer with memory of the given properties.
static void buffer(VkDeviceSize size, VkBufferUsageFlags usage, VkMemoryPropertyFlags props,
                   VkBuffer *buf, VkDeviceMemory *mem) {
	VkBufferCreateInfo info = {
	    .sType = VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO,
	    .size = size,
	    .usage = usage,
	    .sharingMode = VK_SHARING_MODE_EXCLUSIVE,
	};
	CHECK(vkCreateBuffer(dev, &info, NULL, buf));
	VkMemoryRequirements req;
	vkGetBufferMemoryRequirements(dev, *buf, &req);
	VkMemoryAllocateInfo alloc = {
	    .sType = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO,
	    .allocationSize = req.size,
	    .memoryTypeIndex = memory_type(req.memoryTypeBits, props),
	};
	CHECK(vkAllocateMemory(dev, &alloc, NULL, mem));
	CHECK(vkBindBufferMemory(dev, *buf, *mem, 0));
}

// recording is what the command buffer's recording uses.
struct recording {
	VkCommandBuffer cb;
	const VkCommandBufferBeginInfo *begin;
	VkPipeline compute_pipeline;
	VkPipelineLayout compute_layout;
	VkDescriptorSet ds;
	VkRenderPass render_pass;
	VkFramebuffer framebuffer;
	VkPipeline graphics_pipeline;
	VkImage image;
	VkBuffer readback;
};

// record records the compute dispatch and the render, and the copies of
// their results to host-visible memory.
static void record(const struct recording *r) {
	CHECK(vkBeginCommandBuffer(r->cb, r->begin));
	vkCmdBindPipeline(r->cb, VK_PIPELINE_BIND_POINT_COMPUTE, r->compute_pipeline);
	vkCmdBindDescriptorSets(r->cb, VK_PIPELINE_BIND_POINT_COMPUTE, r->compute_layout, 0, 1, &r->ds, 0, NULL);
	vkCmdDispatch(r->cb, N / 64, 1, 1);
	VkClearValue clear = {.color = {.float32 = {0.0f, 0.0f, 1.0f, 1.0f}}};
	VkRenderPassBeginInfo rp_begin = {
	    .sType = VK_STRUCTURE_TYPE_RENDER_PASS_BEGIN_INFO,
	    .renderPass = r->render_pass,
	    .framebuffer = r->framebuffer,
	    .renderArea = {{0, 0}, {W, H}},
	    .clearValueCount = 1,
	    .pClearValues = &clear,
	};
	vkCmdBeginRenderPass(r->cb, &rp_begin, VK_SUBPASS_CONTENTS_INLINE);
	vkCmdBindPipeline(r->cb, VK_PIPELINE_BIND_POINT_GRAPHICS, r->graphics_pipeline);
	vkCmdDraw(r->cb, 6, 1, 0, 0);
	vkCmdEndRenderPass(r->cb);
	VkBufferImageCopy region = {
	    .imageSubresource = {VK_IMAGE_ASPECT_COLOR_BIT, 0, 0, 1},
	    .imageExtent = {W, H, 1},
	};
	vkCmdCopyImageToBuffer(r->cb, r->image, VK_IMAGE_LAYOUT_TRANSFER_SRC_OPTIMAL, r->readback, 1, &region);
	VkMemoryBarrier to_host = {
	    .sType = VK_STRUCTURE_TYPE_MEMORY_BARRIER,
	    .srcAccessMask = VK_ACCESS_SHADER_WRITE_BIT | VK_ACCESS_TRANSFER_WRITE_BIT,
	    .dstAccessMask = VK_ACCESS_HOST_READ_BIT,
	};
	vkCmdPipelineBarrier(r->cb, VK_PIPELINE_STAGE_COMPUTE_SHADER_BIT | VK_PIPELINE_STAGE_TRANSFER_BIT,
	                     VK_PIPELINE_STAGE_HOST_BIT, 0, 1, &to_host, 0, NULL, 0, NULL);
	CHECK(vkEndCommandBuffer(r->cb));
}

int main(int argc, char **argv) {
	if (argc < 4 || argc > 7 || (argc == 7 && strcmp(argv[6], "rerecord") != 0)) {
		fprintf(stderr, "usage: vkoffscreen COMPUTE.spv VERTEX.spv FRAGMENT.spv [ITERATIONS [DELAY_MS [rerecord]]]\n");
		return 2;
	}
	long iterations = argc > 4 ? atol(argv[4]) : 1;
	long delay_ms = argc > 5 ? atol(argv[5]) : 0;
	int rerecord = argc > 6;
	setvbuf(stdout, NULL, _IOLBF, 0);
	VkApplicationInfo app = {
	    .sType = VK_STRUCTURE_TYPE_APPLICATION_INFO,
	    .pApplicationName = "vkoffscreen",
	    .apiVersion = VK_API_VERSION_1_1,
	};
	VkInstanceCreateInfo instance_info = {
	    .sType = VK_STRUCTURE_TYPE_INSTANCE_CREATE_INFO,
	    .pApplicationInfo = &app,
	};
	VkInstance instance;
	CHECK(vkCreateInstance(&instance_info, NULL, &instance));
	uint32_t count = 1;
	VkResult r = vkEnumeratePhysicalDevices(instance, &count, &phys);
	if ((r != VK_SUCCESS && r != VK_INCOMPLETE) || count == 0) {
		fprintf(stderr, "vkoffscreen: no Vulkan device: %d\n", (int)r);
		return 1;
	}
	VkPhysicalDeviceProperties props;
	vkGetPhysicalDeviceProperties(phys, &props);
	printf("vkoffscreen: device %s\n", props.deviceName);

	uint32_t families = 0;
	vkGetPhysicalDeviceQueueFamilyProperties(phys, &families, NULL);
	VkQueueFamilyProperties family_props[16];
	if (families > 16) {
		families = 16;
	}
	vkGetPhysicalDeviceQueueFamilyProperties(phys, &families, family_props);
	uint32_t family = UINT32_MAX;
	for (uint32_t i = 0; i < families; i++) {
		VkQueueFlags want = VK_QUEUE_GRAPHICS_BIT | VK_QUEUE_COMPUTE_BIT;
		if ((family_props[i].queueFlags & want) == want) {
			family = i;
			break;
		}
	}
	if (family == UINT32_MAX) {
		fprintf(stderr, "vkoffscreen: no graphics and compute queue\n");
		return 1;
	}
	float priority = 1.0f;
	VkDeviceQueueCreateInfo queue_info = {
	    .sType = VK_STRUCTURE_TYPE_DEVICE_QUEUE_CREATE_INFO,
	    .queueFamilyIndex = family,
	    .queueCount = 1,
	    .pQueuePriorities = &priority,
	};
	VkDeviceCreateInfo device_info = {
	    .sType = VK_STRUCTURE_TYPE_DEVICE_CREATE_INFO,
	    .queueCreateInfoCount = 1,
	    .pQueueCreateInfos = &queue_info,
	};
	CHECK(vkCreateDevice(phys, &device_info, NULL, &dev));
	VkQueue queue;
	vkGetDeviceQueue(dev, family, 0, &queue);

	VkCommandPoolCreateInfo pool_info = {
	    .sType = VK_STRUCTURE_TYPE_COMMAND_POOL_CREATE_INFO,
	    // Recording again resets the command buffer.
	    .flags = VK_COMMAND_POOL_CREATE_RESET_COMMAND_BUFFER_BIT,
	    .queueFamilyIndex = family,
	};
	VkCommandPool pool;
	CHECK(vkCreateCommandPool(dev, &pool_info, NULL, &pool));
	VkCommandBufferAllocateInfo cb_info = {
	    .sType = VK_STRUCTURE_TYPE_COMMAND_BUFFER_ALLOCATE_INFO,
	    .commandPool = pool,
	    .level = VK_COMMAND_BUFFER_LEVEL_PRIMARY,
	    .commandBufferCount = 1,
	};
	VkCommandBuffer cb;
	CHECK(vkAllocateCommandBuffers(dev, &cb_info, &cb));
	// The loop submits the command buffer again after each submission is
	// done, which one-time submission would not allow.
	VkCommandBufferBeginInfo begin = {
	    .sType = VK_STRUCTURE_TYPE_COMMAND_BUFFER_BEGIN_INFO,
	};
	VkMemoryPropertyFlags host = VK_MEMORY_PROPERTY_HOST_VISIBLE_BIT | VK_MEMORY_PROPERTY_HOST_COHERENT_BIT;

	// Compute: out[i] = i * i + 7.
	VkBuffer compute_buf;
	VkDeviceMemory compute_mem;
	buffer(N * 4, VK_BUFFER_USAGE_STORAGE_BUFFER_BIT, host, &compute_buf, &compute_mem);
	VkDescriptorSetLayoutBinding binding = {
	    .binding = 0,
	    .descriptorType = VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,
	    .descriptorCount = 1,
	    .stageFlags = VK_SHADER_STAGE_COMPUTE_BIT,
	};
	VkDescriptorSetLayoutCreateInfo dsl_info = {
	    .sType = VK_STRUCTURE_TYPE_DESCRIPTOR_SET_LAYOUT_CREATE_INFO,
	    .bindingCount = 1,
	    .pBindings = &binding,
	};
	VkDescriptorSetLayout dsl;
	CHECK(vkCreateDescriptorSetLayout(dev, &dsl_info, NULL, &dsl));
	VkPipelineLayoutCreateInfo pl_info = {
	    .sType = VK_STRUCTURE_TYPE_PIPELINE_LAYOUT_CREATE_INFO,
	    .setLayoutCount = 1,
	    .pSetLayouts = &dsl,
	};
	VkPipelineLayout compute_layout;
	CHECK(vkCreatePipelineLayout(dev, &pl_info, NULL, &compute_layout));
	VkComputePipelineCreateInfo cp_info = {
	    .sType = VK_STRUCTURE_TYPE_COMPUTE_PIPELINE_CREATE_INFO,
	    .stage =
	        {
	            .sType = VK_STRUCTURE_TYPE_PIPELINE_SHADER_STAGE_CREATE_INFO,
	            .stage = VK_SHADER_STAGE_COMPUTE_BIT,
	            .module = shader(argv[1]),
	            .pName = "main",
	        },
	    .layout = compute_layout,
	};
	VkPipeline compute_pipeline;
	CHECK(vkCreateComputePipelines(dev, VK_NULL_HANDLE, 1, &cp_info, NULL, &compute_pipeline));
	VkDescriptorPoolSize pool_size = {
	    .type = VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,
	    .descriptorCount = 1,
	};
	VkDescriptorPoolCreateInfo dp_info = {
	    .sType = VK_STRUCTURE_TYPE_DESCRIPTOR_POOL_CREATE_INFO,
	    .maxSets = 1,
	    .poolSizeCount = 1,
	    .pPoolSizes = &pool_size,
	};
	VkDescriptorPool dpool;
	CHECK(vkCreateDescriptorPool(dev, &dp_info, NULL, &dpool));
	VkDescriptorSetAllocateInfo ds_info = {
	    .sType = VK_STRUCTURE_TYPE_DESCRIPTOR_SET_ALLOCATE_INFO,
	    .descriptorPool = dpool,
	    .descriptorSetCount = 1,
	    .pSetLayouts = &dsl,
	};
	VkDescriptorSet ds;
	CHECK(vkAllocateDescriptorSets(dev, &ds_info, &ds));
	VkDescriptorBufferInfo db_info = {.buffer = compute_buf, .offset = 0, .range = N * 4};
	VkWriteDescriptorSet write = {
	    .sType = VK_STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET,
	    .dstSet = ds,
	    .dstBinding = 0,
	    .descriptorCount = 1,
	    .descriptorType = VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,
	    .pBufferInfo = &db_info,
	};
	vkUpdateDescriptorSets(dev, 1, &write, 0, NULL);

	// Graphics: a 64x64 RGBA8 image, cleared to blue, with a red square.
	VkImageCreateInfo image_info = {
	    .sType = VK_STRUCTURE_TYPE_IMAGE_CREATE_INFO,
	    .imageType = VK_IMAGE_TYPE_2D,
	    .format = VK_FORMAT_R8G8B8A8_UNORM,
	    .extent = {W, H, 1},
	    .mipLevels = 1,
	    .arrayLayers = 1,
	    .samples = VK_SAMPLE_COUNT_1_BIT,
	    .tiling = VK_IMAGE_TILING_OPTIMAL,
	    .usage = VK_IMAGE_USAGE_COLOR_ATTACHMENT_BIT | VK_IMAGE_USAGE_TRANSFER_SRC_BIT,
	    .initialLayout = VK_IMAGE_LAYOUT_UNDEFINED,
	};
	VkImage image;
	CHECK(vkCreateImage(dev, &image_info, NULL, &image));
	VkMemoryRequirements image_req;
	vkGetImageMemoryRequirements(dev, image, &image_req);
	VkMemoryAllocateInfo image_alloc = {
	    .sType = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO,
	    .allocationSize = image_req.size,
	    .memoryTypeIndex = memory_type(image_req.memoryTypeBits, VK_MEMORY_PROPERTY_DEVICE_LOCAL_BIT),
	};
	VkDeviceMemory image_mem;
	CHECK(vkAllocateMemory(dev, &image_alloc, NULL, &image_mem));
	CHECK(vkBindImageMemory(dev, image, image_mem, 0));
	VkImageViewCreateInfo view_info = {
	    .sType = VK_STRUCTURE_TYPE_IMAGE_VIEW_CREATE_INFO,
	    .image = image,
	    .viewType = VK_IMAGE_VIEW_TYPE_2D,
	    .format = VK_FORMAT_R8G8B8A8_UNORM,
	    .subresourceRange = {VK_IMAGE_ASPECT_COLOR_BIT, 0, 1, 0, 1},
	};
	VkImageView view;
	CHECK(vkCreateImageView(dev, &view_info, NULL, &view));
	VkAttachmentDescription attachment = {
	    .format = VK_FORMAT_R8G8B8A8_UNORM,
	    .samples = VK_SAMPLE_COUNT_1_BIT,
	    .loadOp = VK_ATTACHMENT_LOAD_OP_CLEAR,
	    .storeOp = VK_ATTACHMENT_STORE_OP_STORE,
	    .stencilLoadOp = VK_ATTACHMENT_LOAD_OP_DONT_CARE,
	    .stencilStoreOp = VK_ATTACHMENT_STORE_OP_DONT_CARE,
	    .initialLayout = VK_IMAGE_LAYOUT_UNDEFINED,
	    .finalLayout = VK_IMAGE_LAYOUT_TRANSFER_SRC_OPTIMAL,
	};
	VkAttachmentReference color_ref = {0, VK_IMAGE_LAYOUT_COLOR_ATTACHMENT_OPTIMAL};
	VkSubpassDescription subpass = {
	    .pipelineBindPoint = VK_PIPELINE_BIND_POINT_GRAPHICS,
	    .colorAttachmentCount = 1,
	    .pColorAttachments = &color_ref,
	};
	VkRenderPassCreateInfo rp_info = {
	    .sType = VK_STRUCTURE_TYPE_RENDER_PASS_CREATE_INFO,
	    .attachmentCount = 1,
	    .pAttachments = &attachment,
	    .subpassCount = 1,
	    .pSubpasses = &subpass,
	};
	VkRenderPass render_pass;
	CHECK(vkCreateRenderPass(dev, &rp_info, NULL, &render_pass));
	VkFramebufferCreateInfo fb_info = {
	    .sType = VK_STRUCTURE_TYPE_FRAMEBUFFER_CREATE_INFO,
	    .renderPass = render_pass,
	    .attachmentCount = 1,
	    .pAttachments = &view,
	    .width = W,
	    .height = H,
	    .layers = 1,
	};
	VkFramebuffer framebuffer;
	CHECK(vkCreateFramebuffer(dev, &fb_info, NULL, &framebuffer));
	VkPipelineLayoutCreateInfo gpl_info = {.sType = VK_STRUCTURE_TYPE_PIPELINE_LAYOUT_CREATE_INFO};
	VkPipelineLayout graphics_layout;
	CHECK(vkCreatePipelineLayout(dev, &gpl_info, NULL, &graphics_layout));
	VkPipelineShaderStageCreateInfo stages[2] = {
	    {
	        .sType = VK_STRUCTURE_TYPE_PIPELINE_SHADER_STAGE_CREATE_INFO,
	        .stage = VK_SHADER_STAGE_VERTEX_BIT,
	        .module = shader(argv[2]),
	        .pName = "main",
	    },
	    {
	        .sType = VK_STRUCTURE_TYPE_PIPELINE_SHADER_STAGE_CREATE_INFO,
	        .stage = VK_SHADER_STAGE_FRAGMENT_BIT,
	        .module = shader(argv[3]),
	        .pName = "main",
	    },
	};
	VkPipelineVertexInputStateCreateInfo vertex_input = {
	    .sType = VK_STRUCTURE_TYPE_PIPELINE_VERTEX_INPUT_STATE_CREATE_INFO,
	};
	VkPipelineInputAssemblyStateCreateInfo input_assembly = {
	    .sType = VK_STRUCTURE_TYPE_PIPELINE_INPUT_ASSEMBLY_STATE_CREATE_INFO,
	    .topology = VK_PRIMITIVE_TOPOLOGY_TRIANGLE_LIST,
	};
	VkViewport viewport = {0, 0, W, H, 0, 1};
	VkRect2D scissor = {{0, 0}, {W, H}};
	VkPipelineViewportStateCreateInfo viewport_state = {
	    .sType = VK_STRUCTURE_TYPE_PIPELINE_VIEWPORT_STATE_CREATE_INFO,
	    .viewportCount = 1,
	    .pViewports = &viewport,
	    .scissorCount = 1,
	    .pScissors = &scissor,
	};
	VkPipelineRasterizationStateCreateInfo raster = {
	    .sType = VK_STRUCTURE_TYPE_PIPELINE_RASTERIZATION_STATE_CREATE_INFO,
	    .polygonMode = VK_POLYGON_MODE_FILL,
	    .cullMode = VK_CULL_MODE_NONE,
	    .frontFace = VK_FRONT_FACE_COUNTER_CLOCKWISE,
	    .lineWidth = 1.0f,
	};
	VkPipelineMultisampleStateCreateInfo multisample = {
	    .sType = VK_STRUCTURE_TYPE_PIPELINE_MULTISAMPLE_STATE_CREATE_INFO,
	    .rasterizationSamples = VK_SAMPLE_COUNT_1_BIT,
	};
	VkPipelineColorBlendAttachmentState blend_attachment = {
	    .colorWriteMask = VK_COLOR_COMPONENT_R_BIT | VK_COLOR_COMPONENT_G_BIT |
	                      VK_COLOR_COMPONENT_B_BIT | VK_COLOR_COMPONENT_A_BIT,
	};
	VkPipelineColorBlendStateCreateInfo blend = {
	    .sType = VK_STRUCTURE_TYPE_PIPELINE_COLOR_BLEND_STATE_CREATE_INFO,
	    .attachmentCount = 1,
	    .pAttachments = &blend_attachment,
	};
	VkGraphicsPipelineCreateInfo gp_info = {
	    .sType = VK_STRUCTURE_TYPE_GRAPHICS_PIPELINE_CREATE_INFO,
	    .stageCount = 2,
	    .pStages = stages,
	    .pVertexInputState = &vertex_input,
	    .pInputAssemblyState = &input_assembly,
	    .pViewportState = &viewport_state,
	    .pRasterizationState = &raster,
	    .pMultisampleState = &multisample,
	    .pColorBlendState = &blend,
	    .layout = graphics_layout,
	    .renderPass = render_pass,
	};
	VkPipeline graphics_pipeline;
	CHECK(vkCreateGraphicsPipelines(dev, VK_NULL_HANDLE, 1, &gp_info, NULL, &graphics_pipeline));
	VkBuffer readback;
	VkDeviceMemory readback_mem;
	buffer(W * H * 4, VK_BUFFER_USAGE_TRANSFER_DST_BIT, host, &readback, &readback_mem);

	// Record both, to submit together.
	struct recording rec = {
	    .cb = cb,
	    .begin = &begin,
	    .compute_pipeline = compute_pipeline,
	    .compute_layout = compute_layout,
	    .ds = ds,
	    .render_pass = render_pass,
	    .framebuffer = framebuffer,
	    .graphics_pipeline = graphics_pipeline,
	    .image = image,
	    .readback = readback,
	};
	record(&rec);
	VkFenceCreateInfo fence_info = {.sType = VK_STRUCTURE_TYPE_FENCE_CREATE_INFO};
	VkFence fence;
	CHECK(vkCreateFence(dev, &fence_info, NULL, &fence));
	VkSubmitInfo submit = {
	    .sType = VK_STRUCTURE_TYPE_SUBMIT_INFO,
	    .commandBufferCount = 1,
	    .pCommandBuffers = &cb,
	};
	uint32_t *values;
	uint8_t *pixels;
	CHECK(vkMapMemory(dev, compute_mem, 0, N * 4, 0, (void **)&values));
	CHECK(vkMapMemory(dev, readback_mem, 0, W * H * 4, 0, (void **)&pixels));
	int failed = 0;
	uint64_t checksum = 0;
	int red = 0, blue = 0;
	for (long iteration = 1; iteration <= iterations && !failed; iteration++) {
		// Stale results must not pass.
		memset(values, 0, N * 4);
		memset(pixels, 0, W * H * 4);
		CHECK(vkResetFences(dev, 1, &fence));
		if (rerecord && iteration > 1) {
			record(&rec);
		}
		CHECK(vkQueueSubmit(queue, 1, &submit, fence));
		CHECK(vkWaitForFences(dev, 1, &fence, VK_TRUE, 10ull * 1000 * 1000 * 1000));
		for (uint32_t i = 0; i < N; i++) {
			if (values[i] != i * i + 7) {
				fprintf(stderr, "vkoffscreen: compute: out[%u] = %u, want %u\n", i, values[i], i * i + 7);
				failed = 1;
				break;
			}
		}
		checksum = 1469598103934665603ull;
		red = 0;
		blue = 0;
		for (int i = 0; i < W * H * 4; i++) {
			checksum = (checksum ^ pixels[i]) * 1099511628211ull;
		}
		for (int y = 0; y < H; y++) {
			for (int x = 0; x < W; x++) {
				const uint8_t *p = pixels + 4 * (y * W + x);
				int inside = x >= 16 && x < 48 && y >= 16 && y < 48;
				uint8_t want[4] = {inside ? 255 : 0, 0, inside ? 0 : 255, 255};
				if (memcmp(p, want, 4) != 0) {
					if (!failed) {
						fprintf(stderr, "vkoffscreen: render: pixel (%d, %d) = %u,%u,%u,%u\n", x, y,
						        p[0], p[1], p[2], p[3]);
					}
					failed = 1;
				}
				red += inside;
				blue += !inside;
			}
		}
		if (iterations > 1 && !failed) {
			printf("vkoffscreen: iteration %ld ok\n", iteration);
		}
		if (delay_ms > 0 && iteration < iterations) {
			usleep((useconds_t)(delay_ms * 1000));
		}
	}
	vkUnmapMemory(dev, compute_mem);
	vkUnmapMemory(dev, readback_mem);
	vkDestroyDevice(dev, NULL);
	vkDestroyInstance(instance, NULL);
	if (failed) {
		return 1;
	}
	printf("vkoffscreen: ok checksum=%016llx red=%d blue=%d\n", (unsigned long long)checksum, red, blue);
	return 0;
}
