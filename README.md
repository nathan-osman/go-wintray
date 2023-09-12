## go-wintray

A simple package for creating an icon in the system tray and displaying notifications.

### Usage

To use this package, begin by importing it:

```golang
import "github.com/nathan-osman/go-wintray"
```

Next, create the tray icon:

```golang
w := wintray.New()
defer w.Close()
```

You will likely want to set an icon and tooltip:

```golang
//go:embed myapp.ico
var b []byte

w.SetIconFromBytes(b)
w.SetTip("MyApp Is Running")
```

You can add items to the context menu that is shown when the icon is right-clicked:

```golang
w.AddMenuItem("&Print Message", func() {
    fmt.Println("Hello, world!")
})
```

> Note that the provided function will run on a different goroutine than the caller.
