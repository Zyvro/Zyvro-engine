-- Reports which host functions its ctx actually carries. This pack declares
-- every capability, so a correct host gives it all of them.
return {
  type = "reportEveryCapability",
  label = "Report Capabilities",
  category = "Utility",
  description = "Say which host functions this node was given",
  inputs = {},
  outputs = { "json" },

  run = function(ctx)
    return {
      data = {
        llm = ctx.llm ~= nil,
        complete = ctx.complete ~= nil,
        readFile = ctx.readFile ~= nil,
        writeFile = ctx.writeFile ~= nil,
        generateImage = ctx.generateImage ~= nil,
        editImage = ctx.editImage ~= nil,
        removeBackground = ctx.removeBackground ~= nil,
        rotateImage = ctx.rotateImage ~= nil,
        flipImage = ctx.flipImage ~= nil,
        composeImages = ctx.composeImages ~= nil,
        vision = ctx.vision ~= nil,
        brain = ctx.brain ~= nil,
        agentTools = ctx.agentTools ~= nil,
      },
    }
  end,
}
