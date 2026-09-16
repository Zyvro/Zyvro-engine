-- Prints, then fails. It exists so a test can check that what a node printed
-- survives the failure: that is exactly when its author needs to read it.
return {
  type = "noisyFailure",
  label = "Noisy Failure",
  category = "Utility",
  description = "Print two lines and then fail",
  inputs = {},
  outputs = { "text" },

  run = function(ctx)
    print("first line")
    ctx.log("second line")
    error("this node gives up on purpose")
  end,
}
