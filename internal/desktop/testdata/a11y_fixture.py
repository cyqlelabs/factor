# A GTK3 window with known controls at known places, for the accessibility
# live tests. Everything it does is written to the log named on the command
# line, so a test can see an action land from the application's side rather
# than from the tree that reported it.
#
#   python3 a11y_fixture.py LOG X Y
#
# The two large buttons are solid colours the overlay never draws, so a test
# can find them in a screenshot without trusting any coordinate the tree gave.
import sys

import gi

gi.require_version("Gtk", "3.0")
gi.require_version("Gdk", "3.0")
from gi.repository import Gdk, GLib, Gtk  # noqa: E402

log = open(sys.argv[1], "a", buffering=1)
x, y = int(sys.argv[2]), int(sys.argv[3])


def note(s):
    log.write(s + "\n")


css = b"""
#save { background-image: none; background-color: #00c000; color: #000000; border: none; box-shadow: none; }
#cancel { background-image: none; background-color: #0000ff; color: #ffffff; border: none; box-shadow: none; }
"""
provider = Gtk.CssProvider()
provider.load_from_data(css)
Gtk.StyleContext.add_provider_for_screen(Gdk.Screen.get_default(), provider, Gtk.STYLE_PROVIDER_PRIORITY_USER)

win = Gtk.Window(title="Factor Fixture")
win.move(x, y)
win.set_default_size(440, 340)
win.set_resizable(False)

grid = Gtk.Grid(row_spacing=12, column_spacing=12, margin=20)

# The label is beside the field and not wired to it: naming the field is the
# overlay's job, the way it is for every toolkit that never set the relation.
name_label = Gtk.Label(label="Name:")
name = Gtk.Entry()
name.set_text("hello")
name.connect("changed", lambda e: note("entry " + e.get_text()))

secret_label = Gtk.Label(label="Password:")
secret = Gtk.Entry()
secret.set_visibility(False)
secret.set_text("hunter2")

remember = Gtk.CheckButton(label="Remember me")
remember.connect("toggled", lambda c: note("check " + str(c.get_active())))

save = Gtk.Button(label="Save")
save.set_name("save")
save.set_size_request(170, 64)
save.connect("clicked", lambda b: note("clicked Save"))

cancel = Gtk.Button(label="Cancel")
cancel.set_name("cancel")
cancel.set_size_request(170, 64)
cancel.connect("clicked", lambda b: note("clicked Cancel"))

disabled = Gtk.Button(label="Unavailable")
disabled.set_sensitive(False)

grid.attach(name_label, 0, 0, 1, 1)
grid.attach(name, 1, 0, 1, 1)
grid.attach(secret_label, 0, 1, 1, 1)
grid.attach(secret, 1, 1, 1, 1)
grid.attach(remember, 1, 2, 1, 1)
grid.attach(save, 0, 3, 1, 1)
grid.attach(cancel, 1, 3, 1, 1)
grid.attach(disabled, 1, 4, 1, 1)

win.add(grid)
win.connect("destroy", Gtk.main_quit)
win.show_all()
GLib.timeout_add(300, lambda: (note("ready"), False)[1])
Gtk.main()
