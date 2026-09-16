-- Shows the connected images wrapped around a 3D primitive: one image per face
-- of a cube, or as longitude bands of a sphere.
--
-- It runs no model. It is a way of looking at several images at once, which is
-- what you want the moment a workflow starts producing sets of them rather than
-- one at a time.
return {
  type = "voxelPreview",
  label = "3D Voxel Preview",
  category = "Output",
  description = "Cube or sphere textured with the connected images",
  inputs = { "image", "image", "image", "image", "image", "image" },
  outputs = {},
  config = {
    { key = "shape", label = "Shape", type = "select", options = { "cube", "sphere" }, default = "cube" },
  },

  run = function(ctx)
    return ctx.composeImages{ shape = ctx.config.shape }
  end,
}
