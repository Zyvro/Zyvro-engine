-- Reports which host functions its ctx actually carries. The pack declares no
-- capabilities, so a correct host gives it none of them.
return {
  type = "reportCapabilities",
  label = "Report Capabilities",
  category = "Utility",
  description = "Say which host functions this node was given",
  inputs = {},
  outputs = { "json" },

  run = function(ctx)
    return {
      data = {
        llm = ctx.llm ~= nil,
        readFile = ctx.readFile ~= nil,
        writeFile = ctx.writeFile ~= nil,
      },
    }
  end,
}
